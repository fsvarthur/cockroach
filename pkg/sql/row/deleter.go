// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package row

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/valueside"
	"github.com/cockroachdb/cockroach/pkg/sql/rowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// Deleter abstracts the key/value operations for deleting table rows.
type Deleter struct {
	Helper    RowHelper
	FetchCols []catalog.Column
	// FetchColIDtoRowIndex must be kept in sync with FetchCols.
	FetchColIDtoRowIndex catalog.TableColMap
	// For allocation avoidance.
	key         roachpb.Key
	rawValueBuf []byte
}

// MakeDeleter creates a Deleter for the given table.
//
// The returned Deleter contains a FetchCols field that defines the
// expectation of which values are passed as values to DeleteRow. If
// requestedCols is non-nil, then only the requested columns are included in
// FetchCols; otherwise, all columns that are part of the key of any index
// (either primary or secondary) are included in FetchCols.
func MakeDeleter(
	codec keys.SQLCodec,
	tableDesc catalog.TableDescriptor,
	requestedCols []catalog.Column,
	sv *settings.Values,
	internal bool,
	metrics *rowinfra.Metrics,
) Deleter {
	indexes := tableDesc.DeletableNonPrimaryIndexes()

	var fetchCols []catalog.Column
	var fetchColIDtoRowIndex catalog.TableColMap
	if requestedCols != nil {
		fetchCols = requestedCols[:len(requestedCols):len(requestedCols)]
		fetchColIDtoRowIndex = ColIDtoRowIndexFromCols(fetchCols)
	} else {
		maybeAddCol := func(colID descpb.ColumnID) error {
			if _, ok := fetchColIDtoRowIndex.Get(colID); !ok {
				col, err := catalog.MustFindColumnByID(tableDesc, colID)
				if err != nil {
					return err
				}
				fetchColIDtoRowIndex.Set(col.GetID(), len(fetchCols))
				fetchCols = append(fetchCols, col)
			}
			return nil
		}
		for j := 0; j < tableDesc.GetPrimaryIndex().NumKeyColumns(); j++ {
			colID := tableDesc.GetPrimaryIndex().GetKeyColumnID(j)
			if err := maybeAddCol(colID); err != nil {
				return Deleter{}
			}
		}
		for _, index := range indexes {
			for j := 0; j < index.NumKeyColumns(); j++ {
				colID := index.GetKeyColumnID(j)
				if err := maybeAddCol(colID); err != nil {
					return Deleter{}
				}
			}
			// The extra columns are needed to fix #14601.
			for j := 0; j < index.NumKeySuffixColumns(); j++ {
				colID := index.GetKeySuffixColumnID(j)
				if err := maybeAddCol(colID); err != nil {
					return Deleter{}
				}
			}
		}
	}

	rd := Deleter{
		Helper:               NewRowHelper(codec, tableDesc, indexes, nil /* uniqueWithTombstoneIndexes */, sv, internal, metrics),
		FetchCols:            fetchCols,
		FetchColIDtoRowIndex: fetchColIDtoRowIndex,
	}

	return rd
}

// DeleteRow adds to the batch the kv operations necessary to delete a table row
// with the given values.
func (rd *Deleter) DeleteRow(
	ctx context.Context,
	b *kv.Batch,
	values []tree.Datum,
	pm PartialIndexUpdateHelper,
	oth *OriginTimestampCPutHelper,
	traceKV bool,
) error {

	// Delete the row from any secondary indices.
	for i := range rd.Helper.Indexes {
		// If the index ID exists in the set of indexes to ignore, do not
		// attempt to delete from the index.
		if pm.IgnoreForDel.Contains(int(rd.Helper.Indexes[i].GetID())) {
			continue
		}

		// We want to include empty k/v pairs because we want to delete all k/v's for this row.
		entries, err := rowenc.EncodeSecondaryIndex(
			ctx,
			rd.Helper.Codec,
			rd.Helper.TableDesc,
			rd.Helper.Indexes[i],
			rd.FetchColIDtoRowIndex,
			values,
			rowenc.EmptyVectorIndexEncodingHelper,
			true, /* includeEmpty */
		)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := rd.Helper.deleteIndexEntry(ctx, b, rd.Helper.Indexes[i], rd.Helper.secIndexValDirs[i], &e, traceKV); err != nil {
				return err
			}
		}
	}

	primaryIndexKey, err := rd.Helper.encodePrimaryIndexKey(rd.FetchColIDtoRowIndex, values)
	if err != nil {
		return err
	}

	// Delete the row.
	var called bool
	return rd.Helper.TableDesc.ForeachFamily(func(family *descpb.ColumnFamilyDescriptor) error {
		if called {
			// HACK: MakeFamilyKey appends to its argument, so on every loop iteration
			// after the first, trim primaryIndexKey so nothing gets overwritten.
			// TODO(dan): Instead of this, use something like engine.ChunkAllocator.
			primaryIndexKey = primaryIndexKey[:len(primaryIndexKey):len(primaryIndexKey)]
		} else {
			called = true
		}
		familyID := family.ID
		rd.key = keys.MakeFamilyKey(primaryIndexKey, uint32(familyID))

		if oth.IsSet() {
			var expValue []byte
			if !oth.PreviousWasDeleted {
				prevValue, err := rd.encodeValueForPrimaryIndexFamily(family, values)
				if err != nil {
					return err
				}
				if prevValue.IsPresent() {
					expValue = prevValue.TagAndDataBytes()
				}
			}
			oth.DelWithCPut(ctx, &KVBatchAdapter{b}, &rd.key, expValue, traceKV)
		} else {
			if traceKV {
				log.VEventf(ctx, 2, "Del %s", keys.PrettyPrint(rd.Helper.primIndexValDirs, rd.key))
			}
			b.Del(&rd.key)
		}

		rd.key = nil
		return nil
	})
}

// encodeValueForPrimaryIndexFamily encodes the expected roachpb.Value
// for the given family and valuses.
//
// TODO(ssd): Lots of duplication between this and
// prepareInsertOrUpdateBatch. This is rather unfortunate.
func (rd *Deleter) encodeValueForPrimaryIndexFamily(
	family *descpb.ColumnFamilyDescriptor, values []tree.Datum,
) (roachpb.Value, error) {
	if len(family.ColumnIDs) == 1 && family.ColumnIDs[0] == family.DefaultColumnID && family.ID != 0 {
		idx, ok := rd.FetchColIDtoRowIndex.Get(family.DefaultColumnID)
		if !ok {
			return roachpb.Value{}, nil
		}
		if skip, _ := rd.Helper.SkipColumnNotInPrimaryIndexValue(family.DefaultColumnID, values[idx]); skip {
			return roachpb.Value{}, nil
		}
		typ := rd.FetchCols[idx].GetType()
		marshaled, err := valueside.MarshalLegacy(typ, values[idx])
		if err != nil {
			return roachpb.Value{}, err
		}

		return marshaled, err
	}

	rd.rawValueBuf = rd.rawValueBuf[:0]
	familySortedColumnIDs, ok := rd.Helper.SortedColumnFamily(family.ID)
	if !ok {
		return roachpb.Value{}, errors.AssertionFailedf("invalid family sorted column id map")
	}

	var err error
	rd.rawValueBuf, err = rd.Helper.encodePrimaryIndexValuesToBuf(values, rd.FetchColIDtoRowIndex, familySortedColumnIDs, rd.FetchCols, rd.rawValueBuf)
	if err != nil {
		return roachpb.Value{}, err
	}
	ret := roachpb.Value{}
	// For family 0, we expect a value even when no columns have
	// been encoded to oldBytes.
	if family.ID == 0 || len(rd.rawValueBuf) > 0 {
		ret.SetTuple(rd.rawValueBuf)
	}
	return ret, nil
}
