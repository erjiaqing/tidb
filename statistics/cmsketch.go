// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"bytes"
	"math"
	"sort"

	"github.com/cznic/sortutil"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tipb/go-tipb"
	"github.com/spaolacci/murmur3"
)

// topNThreshold is the minimum ratio of the number of topn elements in CMSketch, 10 means 1 / 10 = 10%.
const topNThreshold = 10

// CMSketch is used to estimate point queries.
// Refer: https://en.wikipedia.org/wiki/Count-min_sketch
type CMSketch struct {
	depth        int32
	width        int32
	count        uint64 // TopN is not counted in count
	defaultValue uint64 // In sampled data, if cmsketch returns a small value (less than avg value / 2), then this will returned.
	table        [][]uint32
	topnindex    map[uint64][]cmscount
}

type cmscount struct {
	h1    uint64
	h2    uint64
	data  []byte
	count uint64
}

// NewCMSketch returns a new CM sketch.
func NewCMSketch(d, w int32) *CMSketch {
	tbl := make([][]uint32, d)
	for i := range tbl {
		tbl[i] = make([]uint32, w)
	}
	return &CMSketch{depth: d, width: w, table: tbl}
}

// NewCMSketchWithTopN returns a new CM sketch with TopN elements.
// Total is the size of the whole dataset
// Only when the number of topn elements exceeded len(data) / topnThreshold will store topn elements
func NewCMSketchWithTopN(d, w int32, data [][]byte, n uint32, total uint64) *CMSketch {
	tbl := make([][]uint32, d)
	for i := range tbl {
		tbl[i] = make([]uint32, w)
	}
	c := &CMSketch{depth: d, width: w, table: tbl}
	c.BuildTopN(data, n, topNThreshold, total)
	return c
}

// BuildTopN builds table of top N elements.
// elements in data should not be modified after this call.
// Not exactly n elements, will add a few elements, the number of which is close to the n-th most element.
func (c *CMSketch) BuildTopN(data [][]byte, n, topnThreshold uint32, total uint64) {
	sampleSize := uint64(len(data))
	ratio := total / uint64(sampleSize)
	if total < sampleSize {
		ratio = 1
	}

	counter := make(map[uint64][]cmscount)
	ndv := uint32(0)

	for k := range data {
		h1, h2 := murmur3.Sum128(data[k])
		sl, ok := counter[h1]
		if !ok {
			sl = make([]cmscount, 0)
		}
		exists := false
		for i := range sl {
			if sl[i].h2 == h2 && bytes.Equal(sl[i].data, data[k]) {
				exists = true
				sl[i].count++
			}
		}
		if !exists {
			sl = append(sl, cmscount{h1, h2, data[k], 1})
			ndv++
		}
		counter[h1] = sl
	}

	sorted := make([]uint64, ndv)
	seq := 0

	for _, sl := range counter {
		for i := range sl {
			sorted[seq] = sl[i].count
			seq++
		}
	}

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] > sorted[j]
	})

	if n > ndv {
		n = ndv
	}

	topnCountThreshold := sorted[n-1]
	newTopN := topnCountThreshold
	realTopN := n

	for i := n; i < ndv && i < n*2; i++ {
		if sorted[i]*2 < topnCountThreshold {
			break
		}
		newTopN = sorted[i]
		realTopN = i
	}

	topnCountThreshold = newTopN

	topn := make([]cmscount, realTopN)

	topncount := uint64(0)

	seq = 0
	for _, sl := range counter {
		for i := range sl {
			if uint32(seq) < realTopN && sl[i].count >= topnCountThreshold {
				topn[seq] = sl[i]
				topn[seq].count *= ratio
				topncount += sl[i].count
				seq++
			} else {
				c.InsertBytesN(sl[i].data, sl[i].count*ratio)
			}
		}
	}

	if sampleSize/uint64(topnThreshold) > topncount {
		for i := range topn {
			c.InsertBytesN(topn[i].data, topn[i].count*ratio)
		}
	} else {
		c.buildTopNMap(topn)
	}

	// Interesting, seems we have collected all distinct values.
	// These three tests tests if all divisions are legal.
	// They also tests if we sampled all possible data.
	countWithoutTopN := total - topncount*ratio
	if total < topncount*ratio {
		c.defaultValue = 1
		return
	}

	ndvWithoutTopN := ndv - realTopN
	sampleSizeWithoutTopN := sampleSize - topncount
	if sampleSizeWithoutTopN == 0 {
		c.defaultValue = 1
		return
	}

	estimateNDV := uint64(ndvWithoutTopN) * countWithoutTopN / sampleSizeWithoutTopN
	if estimateNDV == 0 {
		c.defaultValue = 1
		return
	}

	c.defaultValue = countWithoutTopN / estimateNDV
}

func (c *CMSketch) buildTopNMap(topn []cmscount) {
	c.topnindex = make(map[uint64][]cmscount)
	for i := range topn {
		if topn[i].data == nil {
			continue
		}
		h1, h2 := murmur3.Sum128(topn[i].data)
		sl, ok := c.topnindex[h1]
		if !ok {
			sl = make([]cmscount, 0)
		}
		sl = append(sl, cmscount{h1, h2, topn[i].data, topn[i].count})
		c.topnindex[h1] = sl
	}
}

// queryAddTopN TopN adds count to CMSketch.topnindex if exists, and returns the count of such elements after insert
// if such elements does not in topn elements, nothing will happen and false will be returned.
func (c *CMSketch) queryAddTopN(h1, h2, count uint64, d []byte) (uint64, bool) {
	if c.topnindex == nil {
		return 0, false
	}
	cnt, ok := c.topnindex[h1]
	if !ok {
		return 0, false
	}
	for k := range cnt {
		if cnt[k].h2 == h2 && bytes.Equal(d, cnt[k].data) {
			cnt[k].count += count
			return cnt[k].count, true
		}
	}
	return 0, false
}

func (c *CMSketch) queryTopN(h1, h2 uint64, d []byte) (uint64, bool) {
	return c.queryAddTopN(h1, h2, 0, d)
}

// InsertBytes inserts the bytes value into the CM Sketch.
func (c *CMSketch) InsertBytes(bytes []byte) {
	c.InsertBytesN(bytes, 1)
}

// InsertBytesN adds the bytes value into the CM Sketch by n.
func (c *CMSketch) InsertBytesN(bytes []byte, n uint64) {
	h1, h2 := murmur3.Sum128(bytes)
	if _, ok := c.queryAddTopN(h1, h2, n, bytes); ok {
		return
	}
	c.count += n
	for i := range c.table {
		j := (h1 + h2*uint64(i)) % uint64(c.width)
		c.table[i][j] += uint32(n)
	}
}

// setValue sets the count for value that hashed into (h1, h2).
func (c *CMSketch) setValue(h1, h2 uint64, count uint32) {
	oriCount := c.queryHashValue(h1, h2)

	if oriCount < 2*(c.count/uint64(c.width)) && c.defaultValue > 0 {
		// This case, we should also update c.defaultValue
		// Set default value directly will result in more error, instead, update it by 5%.
		c.defaultValue = uint64(float64(c.defaultValue)*0.95 + float64(c.defaultValue)*0.05)
		if c.defaultValue == 0 {
			// c.defaultValue never guess 0 since we are using a sampled data, instead, return a small number, like 1.
			c.defaultValue = 1
		}
	}

	c.count += uint64(count) - oriCount
	// let it overflow naturally
	deltaCount := count - uint32(oriCount)
	for i := range c.table {
		j := (h1 + h2*uint64(i)) % uint64(c.width)
		c.table[i][j] = c.table[i][j] + deltaCount
	}
}

// setValue sets the count for value that hashed into (h1, h2).
func (c *CMSketch) setValueBytes(d []byte, count uint64) {
	h1, h2 := murmur3.Sum128(d)
	if oriCount, ok := c.queryTopN(h1, h2, d); ok {
		deltaCount := count - oriCount
		c.queryAddTopN(h1, h2, deltaCount, d)
	} else {
		c.setValue(h1, h2, uint32(count))
	}
}

func (c *CMSketch) queryValue(sc *stmtctx.StatementContext, val types.Datum) (uint64, error) {
	bytes, err := codec.EncodeValue(sc, nil, val)
	if err != nil {
		return 0, errors.Trace(err)
	}
	return c.QueryBytes(bytes), nil
}

// QueryBytes is used to query the count of specified bytes.
func (c *CMSketch) QueryBytes(d []byte) uint64 {
	h1, h2 := murmur3.Sum128(d)
	if count, ok := c.queryTopN(h1, h2, d); ok {
		return count
	}
	return c.queryHashValue(h1, h2)
}

func (c *CMSketch) queryHashValue(h1, h2 uint64) uint64 {
	vals := make([]uint32, c.depth)
	min := uint32(math.MaxUint32)
	for i := range c.table {
		j := (h1 + h2*uint64(i)) % uint64(c.width)
		if min > c.table[i][j] {
			min = c.table[i][j]
		}
		noise := (c.count - uint64(c.table[i][j])) / (uint64(c.width) - 1)
		if uint64(c.table[i][j]) < noise {
			vals[i] = 0
		} else {
			vals[i] = c.table[i][j] - uint32(noise)
		}
	}
	sort.Sort(sortutil.Uint32Slice(vals))
	res := vals[(c.depth-1)/2] + (vals[c.depth/2]-vals[(c.depth-1)/2])/2
	if res > min {
		res = min
	}
	// If res is small than some value, we think it is sampled occasionally
	if res < 2*uint32((c.count/uint64(c.width))) && c.defaultValue > 0 {
		// Assume items not in CMSketch is a average value
		return c.defaultValue
	}
	return uint64(res)
}

// MergeCMSketch merges two CM Sketch.
// Call with CMSketch with top N initialized may downgrade the result
func (c *CMSketch) MergeCMSketch(rc *CMSketch) error {
	if c.depth != rc.depth || c.width != rc.width {
		return errors.New("Dimensions of Count-Min Sketch should be the same")
	}
	if c.topnindex != nil || rc.topnindex != nil {
		return errors.New("CMSketch with top n does not supports merge")
	}
	c.count += rc.count
	for i := range c.table {
		for j := range c.table[i] {
			c.table[i][j] += rc.table[i][j]
		}
	}
	return nil
}

// CMSketchToProto converts CMSketch to its protobuf representation.
func CMSketchToProto(c *CMSketch) *tipb.CMSketch {
	protoSketch := &tipb.CMSketch{Rows: make([]*tipb.CMSketchRow, c.depth)}
	for i := range c.table {
		protoSketch.Rows[i] = &tipb.CMSketchRow{Counters: make([]uint32, c.width)}
		for j := range c.table[i] {
			protoSketch.Rows[i].Counters[j] = c.table[i][j]
		}
	}
	if c.topnindex != nil {
		protoSketch.TopN = make([]*tipb.CMSketchTopN, len(c.topnindex))
		seq := uint32(0)
		for _, v1 := range c.topnindex {
			for _, v := range v1 {
				protoSketch.TopN[seq] = &tipb.CMSketchTopN{Data: v.data, Count: v.count}
				seq++
			}
		}
	}
	return protoSketch
}

// CMSketchFromProto converts CMSketch from its protobuf representation.
// If topn is broken, we cannot recover the top n elements
func CMSketchFromProto(protoSketch *tipb.CMSketch) (*CMSketch, error) {
	if protoSketch == nil {
		return nil, nil
	}
	c := NewCMSketch(int32(len(protoSketch.Rows)), int32(len(protoSketch.Rows[0].Counters)))
	for i, row := range protoSketch.Rows {
		c.count = 0
		for j, counter := range row.Counters {
			c.table[i][j] = counter
			c.count = c.count + uint64(counter)
		}
	}
	if protoSketch.TopN != nil {
		// Check if we fill the Data field
		for p := range protoSketch.TopN {
			if protoSketch.TopN[p].Data == nil {
				return nil, errors.New("Broken data")
			}
		}
		c.topnindex = make(map[uint64][]cmscount)
		topn := make([]cmscount, len(protoSketch.TopN))
		for p, v := range protoSketch.TopN {
			h1, h2 := murmur3.Sum128(v.Data)
			topn[p] = cmscount{h1, h2, v.Data, v.Count}
		}
		c.buildTopNMap(topn)
	}
	return c, nil
}

func encodeCMSketch(c *CMSketch) ([]byte, [][]byte, error) {
	if c == nil || c.count == 0 {
		return nil, nil, nil
	}
	p := CMSketchToProto(c)
	var topn [][]byte
	if p.TopN != nil {
		topn = make([][]byte, len(p.TopN))
		for i := range p.TopN {
			topn[i] = p.TopN[i].Data
			// Do not store actual Top N data into protobuf.
			p.TopN[i].Data = nil
		}
	}
	r, err := p.Marshal()
	return r, topn, err
}

func decodeCMSketch(data []byte, topn [][]byte) (*CMSketch, error) {
	if data == nil {
		return nil, nil
	}
	p := &tipb.CMSketch{}
	err := p.Unmarshal(data)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(p.Rows) == 0 {
		return nil, nil
	}
	if len(topn) == len(p.TopN) {
		for i := range topn {
			p.TopN[i].Data = topn[i]
		}
	}
	return CMSketchFromProto(p)
}

// TotalCount returns the count, it is only used for test.
func (c *CMSketch) TotalCount() uint64 {
	return c.count
}

// Equal tests if two CM Sketch equal, it is only used for test.
func (c *CMSketch) Equal(rc *CMSketch) bool {
	if c == nil || rc == nil {
		return c == nil && rc == nil
	}
	if c.width != rc.width || c.depth != rc.depth || c.count != rc.count || c.defaultValue != rc.defaultValue {
		return false
	}
	for i := range c.table {
		for j := range c.table[i] {
			if c.table[i][j] != rc.table[i][j] {
				return false
			}
		}
	}
	if len(rc.topnindex) > 0 || len(c.topnindex) > 0 {
		if len(rc.topnindex) != len(c.topnindex) {
			return false
		}
		for k, v := range c.topnindex {
			v2, ok := rc.topnindex[k]
			if !ok || len(v) != len(v2) {
				return false
			}
			for i := range v {
				hasMatch := false
				for j := range v2 {
					if v[i].h2 == v2[j].h2 && bytes.Equal(v[i].data, v2[j].data) && v[i].count == v2[j].count {
						hasMatch = true
						break
					}
				}
				if !hasMatch {
					return false
				}
			}
		}
	}
	return true
}

// Copy makes a deepcopy of CMSketch
// This function should not used outside this package
// Make it public for handle_test
func (c *CMSketch) Copy() *CMSketch {
	if c == nil {
		return nil
	}
	tbl := make([][]uint32, c.depth)
	for i := range tbl {
		tbl[i] = make([]uint32, c.width)
		copy(tbl[i], c.table[i])
	}
	var ntopn map[uint64][]cmscount
	if c.topnindex != nil {
		ntopn = make(map[uint64][]cmscount)
		for k, v := range c.topnindex {
			newSlice := make([]cmscount, len(v))
			for i := range v {
				newSlice[i] = cmscount{v[i].h1, v[i].h2, make([]byte, len(v[i].data)), v[i].count}
				copy(newSlice[i].data, v[i].data)
			}
			ntopn[k] = newSlice
		}
	}
	return &CMSketch{count: c.count, width: c.width, depth: c.depth, table: tbl, topnindex: ntopn}
}
