package vparquet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/segmentio/parquet-go"
	"github.com/willf/bloom"

	tempo_io "github.com/grafana/tempo/pkg/io"
	pq "github.com/grafana/tempo/pkg/parquetquery"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

const (
	SearchPrevious = -1
	SearchNext     = -2
	NotFound       = -3

	TraceIDColumnName = "TraceID"
)

type RowTracker struct {
	rgs         []parquet.RowGroup
	startRowNum []int

	// traceID column index
	colIndex int
}

// Scanning for a traceID within a rowGroup. Parameters are the rowgroup number and traceID to be searched.
// Includes logic to look through bloom filters and page bounds as it goes through the rowgroup.
func (rt *RowTracker) findTraceByID(idx int, traceID string) int {
	rgIdx := rt.rgs[idx]
	rowMatch := int64(rt.startRowNum[idx])
	traceIDColumnChunk := rgIdx.ColumnChunks()[rt.colIndex]

	bf := traceIDColumnChunk.BloomFilter()
	if bf != nil {
		// todo: better error handling?
		exists, _ := bf.Check(parquet.ValueOf(traceID))
		if !exists {
			return NotFound
		}
	}

	// get row group bounds
	numPages := traceIDColumnChunk.ColumnIndex().NumPages()
	min := traceIDColumnChunk.ColumnIndex().MinValue(0).String()
	max := traceIDColumnChunk.ColumnIndex().MaxValue(numPages - 1).String()
	if strings.Compare(traceID, min) < 0 {
		return SearchPrevious
	}
	if strings.Compare(max, traceID) < 0 {
		return SearchNext
	}

	pages := traceIDColumnChunk.Pages()
	for {
		pg, err := pages.ReadPage()
		if pg == nil || err == io.EOF {
			break
		}

		if min, max, ok := pg.Bounds(); ok {
			if strings.Compare(traceID, min.String()) < 0 {
				return SearchPrevious
			}
			if strings.Compare(max.String(), traceID) < 0 {
				rowMatch += pg.NumRows()
				continue
			}
		}

		vr := pg.Values()
		for {
			vs := make([]parquet.Value, 1000)
			x, err := vr.ReadValues(vs)
			for y := 0; y < x; y++ {
				if strings.Compare(vs[y].String(), traceID) == 0 {
					rowMatch += int64(y)
					return int(rowMatch)
				}
			}

			// check for EOF after processing any returned data
			if err == io.EOF {
				break
			}
			// todo: better error handling
			if err != nil {
				break
			}

			rowMatch += int64(x)
		}
		break
	}

	// did not find the trace
	return NotFound
}

// Simple binary search algorithm over the parquet rowgroups to efficiently
// search for traceID in the block (works only because rows are sorted by traceID)
func (rt *RowTracker) binarySearch(start int, end int, traceID string) int {
	if start > end {
		return -1
	}

	// check mid point
	midResult := rt.findTraceByID((start+end)/2, traceID)
	if midResult == SearchPrevious {
		return rt.binarySearch(start, ((start+end)/2)-1, traceID)
	} else if midResult < 0 {
		return rt.binarySearch(((start+end)/2)+1, end, traceID)
	}

	return midResult
}

func (b *backendBlock) checkBloom(ctx context.Context, id common.ID) (found bool, err error) {
	span, derivedCtx := opentracing.StartSpanFromContext(ctx, "parquet.backendBlock.checkBloom",
		opentracing.Tags{
			"blockID":  b.meta.BlockID,
			"tenantID": b.meta.TenantID,
		})
	defer span.Finish()

	shardKey := common.ShardKeyForTraceID(id, int(b.meta.BloomShardCount))
	nameBloom := common.BloomName(shardKey)
	span.SetTag("bloom", nameBloom)

	bloomBytes, err := b.r.Read(derivedCtx, nameBloom, b.meta.BlockID, b.meta.TenantID, true)
	if err != nil {
		return false, fmt.Errorf("error retrieving bloom (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	filter := &bloom.BloomFilter{}
	_, err = filter.ReadFrom(bytes.NewReader(bloomBytes))
	if err != nil {
		return false, fmt.Errorf("error parsing bloom (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	return filter.Test(id), nil
}

func (b *backendBlock) FindTraceByID(ctx context.Context, id common.ID) (_ *tempopb.Trace, err error) {
	span, derivedCtx := opentracing.StartSpanFromContext(ctx, "parquet.backendBlock.FindTraceByID",
		opentracing.Tags{
			"blockID":   b.meta.BlockID,
			"tenantID":  b.meta.TenantID,
			"blockSize": b.meta.Size,
		})
	defer span.Finish()

	found, err := b.checkBloom(derivedCtx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	traceID := util.TraceIDToHexString(id)

	rr := NewBackendReaderAt(derivedCtx, b.r, DataFileName, b.meta.BlockID, b.meta.TenantID)
	defer func() { span.SetTag("inspectedBytes", rr.TotalBytesRead) }()

	br := tempo_io.NewBufferedReaderAt(rr, int64(b.meta.Size), 512*1024, 32)

	pf, err := parquet.OpenFile(br, int64(b.meta.Size))
	if err != nil {
		return nil, errors.Wrap(err, "error opening file in FindTraceByID")
	}

	// traceID column index
	colIndex, _ := pq.GetColumnIndexByPath(pf, TraceIDColumnName)

	numRowGroups := len(pf.RowGroups())
	rt := &RowTracker{
		rgs:         make([]parquet.RowGroup, 0, numRowGroups),
		startRowNum: make([]int, 0, numRowGroups),

		colIndex: colIndex,
	}

	rowCount := 0
	for rgi := 0; rgi < numRowGroups; rgi++ {
		rt.rgs = append(rt.rgs, pf.RowGroups()[rgi])
		rt.startRowNum = append(rt.startRowNum, rowCount)
		rowCount += int(pf.RowGroups()[rgi].NumRows())
	}

	// find row number of matching traceID
	rowMatch := rt.binarySearch(0, numRowGroups-1, traceID)

	// traceID not found in this block
	if rowMatch < 0 {
		return nil, nil
	}

	// seek to row and read
	tr := new(Trace)
	sch := parquet.SchemaOf(tr)
	r := parquet.NewReader(pf, sch)
	var row parquet.Row

	r.SeekToRow(int64(rowMatch))
	row, err = r.ReadRow(row)
	if err != nil {
		return nil, errors.Wrap(err, "error reading row from backend")
	}

	fmt.Printf("Found trace id: %s in parquet block %v at row %d\n", traceID, b.meta.BlockID, rowMatch)

	// HACK: something isn't working with SeekToRow
	// so instead read rows up to the one we need
	/*
		r := parquet.NewReader(pf, sch)

		for i := 0; i <= rowMatch; i++ {
			row, err = r.ReadRow(row[:0])
			if err != nil {
				return nil, errors.Wrap(err, fmt.Sprint("error reading row from backend: row number:", i))
			}
		}
	*/

	for i, v := range row {
		fmt.Printf("row[%d] = c:%d v:%v r:%d d:%d\n", i, v.Column(), v.String(), v.RepetitionLevel(), v.DefinitionLevel())
	}
	err = sch.Reconstruct(tr, row)
	if err != nil {
		return nil, errors.Wrap(err, "error reading row from backend")
	}

	// convert to proto trace and return
	return parquetTraceToTempopbTrace(tr)
}
