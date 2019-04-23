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

	"github.com/cznic/mathutil"
	"github.com/cznic/sortutil"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/hack"
	"github.com/pingcap/tipb/go-tipb"
	"github.com/spaolacci/murmur3"
)

// topNThreshold is the minimum ratio of the number of topn elements in CMSketch, 10 means 1 / 10 = 10%.
const topNThreshold = uint64(10)

// CMSketch is used to estimate point queries.
// Refer: https://en.wikipedia.org/wiki/Count-min_sketch
type CMSketch struct {
	depth        int32
	width        int32
	count        uint64 // TopN is not counted in count
	defaultValue uint64 // In sampled data, if cmsketch returns a small value (less than avg value / 2), then this will returned.
	table        [][]uint32
	topN         map[uint64][]dataCount
}

// dataCount is a simple counter used by BuildTopN
type dataCount struct {
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

// topNHelper wraps some variables used when building cmsketch with top n.
type topNHelper struct {
	sampleSize    uint64
	numTop        uint32
	counter       map[hack.MutableString]uint64
	sorted        []uint64
	onlyOnceItems uint64
	sumTopN       uint64
	lastVal       uint64
}

func newTopNHelper(data [][]byte, numTop uint32) *topNHelper {
	counter := make(map[hack.MutableString]uint64)
	for k := range data {
		counter[hack.String(data[k])]++
	}
	sorted, onlyOnceItems := make([]uint64, 0, len(counter)), uint64(0)
	for _, cnt := range counter {
		sorted = append(sorted, cnt)
		if cnt == 1 {
			onlyOnceItems++
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] > sorted[j]
	})

	var (
		// last is the last element in top N index should occurres atleast `last` times.
		last      uint64
		sumTopN   uint64
		sampleNDV = uint32(len(sorted))
	)
	numTop = mathutil.MinUint32(sampleNDV, numTop) // In case numTop is bigger than sample NUV.
	// The following loop builds find how many elements be added to the top N index.
	// The final Top-N index may have at most 2*numTop elements for some less skewed data.
	for i := uint32(0); i < sampleNDV && i < numTop*2; i++ {
		// Here, 2/3 is get by running tests, tested 1, 1/2, 2/3, and 2/3 is relative better than 1 and 1/2.
		// If the frequency of i-th elements is close to n-th element, it is added to the top N index too.
		if i >= numTop && sorted[i]*3 < sorted[numTop-1]*2 && last != sorted[i] {
			break
		}
		// These two values are only used for build topNIndex, and they are not used in counting defaultValue.
		last = sorted[i]
		// We use sumTopN to estimate the total count of top N elements to determine weather to build the top N index.
		sumTopN += sorted[i]
	}

	return &topNHelper{uint64(len(data)), numTop, counter, sorted, onlyOnceItems, sumTopN, last}
}

// NewCMSketchWithTopN returns a new CM sketch with TopN elements.
// total is the size of the whole dataset
func NewCMSketchWithTopN(d, w int32, data [][]byte, numTop uint32, rowCount uint64) *CMSketch {
	helper := newTopNHelper(data, numTop)
	estimateNDV, ratio := calculateEstimateNDV(helper, rowCount)
	c := helper.buildCMSWithTopN(d, w, ratio)
	c.calculateDefaultVal(helper, estimateNDV, ratio, rowCount)
	return c
}

// finalBuild builds Top-N and cmsketch
func (helper *topNHelper) buildCMSWithTopN(d, w int32, ratio uint64) (c *CMSketch) {
	c = NewCMSketch(d, w)
	enableTopN := helper.sampleSize/topNThreshold <= helper.sumTopN
	topN := make([]dataCount, 0, helper.numTop)
	helper.sumTopN = 0
	for counterKey, cnt := range helper.counter {
		if enableTopN && cnt >= helper.lastVal {
			topN = append(topN, dataCount{data: hack.Slice(string(counterKey)), count: cnt * ratio})
			helper.sumTopN += cnt * ratio
		} else {
			c.updateBytesWithDelta(hack.Slice(string(counterKey)), cnt*ratio)
		}
	}
	if !enableTopN {
		return
	}
	helper.numTop = uint32(len(topN))
	c.topN = make(map[uint64][]dataCount)
	for i := range topN {
		if topN[i].data == nil {
			continue
		}
		h1, h2 := murmur3.Sum128(topN[i].data)
		vals, ok := c.topN[h1]
		if !ok {
			vals = make([]dataCount, 0)
		}
		vals = append(vals, dataCount{h1, h2, topN[i].data, topN[i].count})
		c.topN[h1] = vals
	}
	return
}

func (c *CMSketch) calculateDefaultVal(helper *topNHelper, estimateNDV, ratio, rowCount uint64) {
	sampleNDV := uint64(len(helper.sorted))
	if rowCount <= helper.sumTopN {
		c.defaultValue = 1
	} else if estimateNDV <= uint64(helper.numTop) {
		c.defaultValue = 1
	} else if estimateNDV+helper.onlyOnceItems <= uint64(sampleNDV) {
		c.defaultValue = 1
	} else {
		estimateRemainingCount := rowCount - (helper.sampleSize-uint64(helper.onlyOnceItems))*ratio
		c.defaultValue = estimateRemainingCount / (estimateNDV - uint64(sampleNDV) + helper.onlyOnceItems)
	}
}

// queryAddTopN TopN adds count to CMSketch.topNIndex if exists, and returns the count of such elements after insert
// if such elements does not in topn elements, nothing will happen and false will be returned.
func (c *CMSketch) updateTopNWithDelta(h1, h2, delta uint64, d []byte) bool {
	if c.topN == nil {
		return false
	}
	for _, cnt := range c.topN[h1] {
		if cnt.h2 == h2 && bytes.Equal(d, cnt.data) {
			cnt.count += delta
			return true
		}
	}
	return false
}

func (c *CMSketch) queryTopN(h1, h2 uint64, d []byte) (uint64, bool) {
	if c.topN == nil {
		return 0, false
	}
	for _, cnt := range c.topN[h1] {
		if cnt.h2 == h2 && bytes.Equal(d, cnt.data) {
			return cnt.count, true
		}
	}
	return 0, false
}

// InsertBytes inserts the bytes value into the CM Sketch.
func (c *CMSketch) InsertBytes(bytes []byte) {
	c.updateBytesWithDelta(bytes, 1)
}

// insertBytesN adds the bytes value into the CM Sketch by n.
func (c *CMSketch) updateBytesWithDelta(bytes []byte, n uint64) {
	h1, h2 := murmur3.Sum128(bytes)
	if c.updateTopNWithDelta(h1, h2, n, bytes) {
		return
	}
	c.count += n
	for i := range c.table {
		j := (h1 + h2*uint64(i)) % uint64(c.width)
		c.table[i][j] += uint32(n)
	}
}

func (c *CMSketch) considerDefVal(cnt uint64) bool {
	return cnt < 2*(c.count/uint64(c.width)) && c.defaultValue > 0
}

// setValue sets the count for value that hashed into (h1, h2).
func (c *CMSketch) setValue(h1, h2 uint64, count uint32) {
	oriCount := c.queryHashValue(h1, h2)

	if c.considerDefVal(oriCount) && c.defaultValue > 0 {
		// This case, we should also update c.defaultValue
		// Set default value directly will result in more error, instead, update it by 5%.
		// This should make estimate better, if defaultValue becomes 0 frequently, commit this line.
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
	if c.considerDefVal(uint64(res)) {
		// Assume items not in CMSketch is a average value
		return c.defaultValue
	}
	return uint64(res)
}

// MergeCMSketch merges two CM Sketch.
// Call with CMSketch with Top-N initialized may downgrade the result
func (c *CMSketch) MergeCMSketch(rc *CMSketch) error {
	if c.depth != rc.depth || c.width != rc.width {
		return errors.New("Dimensions of Count-Min Sketch should be the same")
	}
	if c.topN != nil || rc.topN != nil {
		return errors.New("CMSketch with Top-N does not supports merge")
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
// TODO: Encode/Decode cmsketch with Top-N
func CMSketchToProto(c *CMSketch) *tipb.CMSketch {
	protoSketch := &tipb.CMSketch{Rows: make([]*tipb.CMSketchRow, c.depth)}
	for i := range c.table {
		protoSketch.Rows[i] = &tipb.CMSketchRow{Counters: make([]uint32, c.width)}
		for j := range c.table[i] {
			protoSketch.Rows[i].Counters[j] = c.table[i][j]
		}
	}
	return protoSketch
}

// CMSketchFromProto converts CMSketch from its protobuf representation.
// TODO: Encode/Decode cmsketch with Top-N
func CMSketchFromProto(protoSketch *tipb.CMSketch) *CMSketch {
	if protoSketch == nil {
		return nil
	}
	c := NewCMSketch(int32(len(protoSketch.Rows)), int32(len(protoSketch.Rows[0].Counters)))
	for i, row := range protoSketch.Rows {
		c.count = 0
		for j, counter := range row.Counters {
			c.table[i][j] = counter
			c.count = c.count + uint64(counter)
		}
	}
	return c
}

// EncodeCMSketch encodes the given CMSketch to byte slice.
func EncodeCMSketch(c *CMSketch) ([]byte, error) {
	if c == nil || c.count == 0 {
		return nil, nil
	}
	p := CMSketchToProto(c)
	return p.Marshal()
}

// DecodeCMSketch decode a CMSketch from the given byte slice.
func DecodeCMSketch(data []byte) (*CMSketch, error) {
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
	return CMSketchFromProto(p), nil
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
	if c.width != rc.width || c.depth != rc.depth || c.count != rc.count {
		return false
	}
	for i := range c.table {
		for j := range c.table[i] {
			if c.table[i][j] != rc.table[i][j] {
				return false
			}
		}
	}
	return true
}

// Copy makes a copy for current CMSketch.
func (c *CMSketch) Copy() *CMSketch {
	if c == nil {
		return nil
	}
	tbl := make([][]uint32, c.depth)
	for i := range tbl {
		tbl[i] = make([]uint32, c.width)
		copy(tbl[i], c.table[i])
	}
	return &CMSketch{count: c.count, width: c.width, depth: c.depth, table: tbl}
}
