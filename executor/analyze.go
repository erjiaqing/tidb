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

package executor

import (
	"context"
	"runtime"
	"strconv"
	"sync"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

var _ Executor = &AnalyzeExec{}

// AnalyzeExec represents Analyze executor.
type AnalyzeExec struct {
	baseExecutor
	tasks []*analyzeTask
}

const (
	maxSampleSize        = 10000
	maxRegionSampleSize  = 1000
	maxSketchSize        = 10000
	defaultCMSketchDepth = 5
	defaultCMSketchWidth = 2048
)

// Next implements the Executor Next interface.
func (e *AnalyzeExec) Next(ctx context.Context, req *chunk.RecordBatch) error {
	concurrency, err := getBuildStatsConcurrency(e.ctx)
	if err != nil {
		return err
	}
	taskCh := make(chan *analyzeTask, len(e.tasks))
	resultCh := make(chan analyzeResult, len(e.tasks))
	for i := 0; i < concurrency; i++ {
		go e.analyzeWorker(taskCh, resultCh)
	}
	for _, task := range e.tasks {
		statistics.AddNewAnalyzeJob(task.job)
	}
	for _, task := range e.tasks {
		taskCh <- task
	}
	close(taskCh)
	statsHandle := domain.GetDomain(e.ctx).StatsHandle()
	for i, panicCnt := 0, 0; i < len(e.tasks) && panicCnt < concurrency; i++ {
		result := <-resultCh
		if result.Err != nil {
			err = result.Err
			if err == errAnalyzeWorkerPanic {
				panicCnt++
			} else {
				logutil.Logger(ctx).Error("analyze failed", zap.Error(err))
			}
			result.job.Finish(true)
			continue
		}
		for i, hg := range result.Hist {
			err1 := statsHandle.SaveStatsToStorage(result.PhysicalTableID, result.Count, result.IsIndex, hg, result.Cms[i], 1)
			if err1 != nil {
				err = err1
				logutil.Logger(ctx).Error("save stats to storage failed", zap.Error(err))
				result.job.Finish(true)
				continue
			}
		}
		result.job.Finish(false)
	}
	for _, task := range e.tasks {
		statistics.MoveToHistory(task.job)
	}
	if err != nil {
		return err
	}
	return statsHandle.Update(GetInfoSchema(e.ctx))
}

func getBuildStatsConcurrency(ctx sessionctx.Context) (int, error) {
	sessionVars := ctx.GetSessionVars()
	concurrency, err := variable.GetSessionSystemVar(sessionVars, variable.TiDBBuildStatsConcurrency)
	if err != nil {
		return 0, err
	}
	c, err := strconv.ParseInt(concurrency, 10, 64)
	return int(c), err
}

type taskType int

const (
	colTask taskType = iota
	idxTask
	fastTask
)

type analyzeTask struct {
	taskType taskType
	idxExec  *AnalyzeIndexExec
	colExec  *AnalyzeColumnsExec
	fastExec *AnalyzeFastExec
	job      *statistics.AnalyzeJob
}

var errAnalyzeWorkerPanic = errors.New("analyze worker panic")

func (e *AnalyzeExec) analyzeWorker(taskCh <-chan *analyzeTask, resultCh chan<- analyzeResult) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			stackSize := runtime.Stack(buf, false)
			buf = buf[:stackSize]
			logutil.Logger(context.Background()).Error("analyze worker panicked", zap.String("stack", string(buf)))
			metrics.PanicCounter.WithLabelValues(metrics.LabelAnalyze).Inc()
			resultCh <- analyzeResult{
				Err: errAnalyzeWorkerPanic,
			}
		}
	}()
	for task := range taskCh {
		switch task.taskType {
		case colTask:
			task.colExec.job = task.job
			task.job.Start()
			resultCh <- analyzeColumnsPushdown(task.colExec)
		case idxTask:
			task.idxExec.job = task.job
			task.job.Start()
			resultCh <- analyzeIndexPushdown(task.idxExec)
		case fastTask:
			for _, result := range analyzeFastExec(task.fastExec) {
				resultCh <- result
			}
		}
	}
}

func analyzeIndexPushdown(idxExec *AnalyzeIndexExec) analyzeResult {
	hist, cms, err := idxExec.buildStats()
	if err != nil {
		return analyzeResult{Err: err, job: idxExec.job}
	}
	result := analyzeResult{
		PhysicalTableID: idxExec.physicalTableID,
		Hist:            []*statistics.Histogram{hist},
		Cms:             []*statistics.CMSketch{cms},
		IsIndex:         1,
		job:             idxExec.job,
	}
	result.Count = hist.NullCount
	if hist.Len() > 0 {
		result.Count += hist.Buckets[hist.Len()-1].Count
	}
	return result
}

// AnalyzeIndexExec represents analyze index push down executor.
type AnalyzeIndexExec struct {
	ctx             sessionctx.Context
	physicalTableID int64
	idxInfo         *model.IndexInfo
	concurrency     int
	priority        int
	analyzePB       *tipb.AnalyzeReq
	result          distsql.SelectResult
	countNullRes    distsql.SelectResult
	maxNumBuckets   uint64
	job             *statistics.AnalyzeJob
}

// fetchAnalyzeResult builds and dispatches the `kv.Request` from given ranges, and stores the `SelectResult`
// in corresponding fields based on the input `isNullRange` argument, which indicates if the range is the
// special null range for single-column index to get the null count.
func (e *AnalyzeIndexExec) fetchAnalyzeResult(ranges []*ranger.Range, isNullRange bool) error {
	var builder distsql.RequestBuilder
	kvReq, err := builder.SetIndexRanges(e.ctx.GetSessionVars().StmtCtx, e.physicalTableID, e.idxInfo.ID, ranges).
		SetAnalyzeRequest(e.analyzePB).
		SetKeepOrder(true).
		SetConcurrency(e.concurrency).
		Build()
	if err != nil {
		return err
	}
	ctx := context.TODO()
	result, err := distsql.Analyze(ctx, e.ctx.GetClient(), kvReq, e.ctx.GetSessionVars().KVVars, e.ctx.GetSessionVars().InRestrictedSQL)
	if err != nil {
		return err
	}
	result.Fetch(ctx)
	if isNullRange {
		e.countNullRes = result
	} else {
		e.result = result
	}
	return nil
}

func (e *AnalyzeIndexExec) open() error {
	ranges := ranger.FullRange()
	// For single-column index, we do not load null rows from TiKV, so the built histogram would not include
	// null values, and its `NullCount` would be set by result of another distsql call to get null rows.
	// For multi-column index, we cannot define null for the rows, so we still use full range, and the rows
	// containing null fields would exist in built histograms. Note that, the `NullCount` of histograms for
	// multi-column index is always 0 then.
	if len(e.idxInfo.Columns) == 1 {
		ranges = ranger.FullNotNullRange()
	}
	err := e.fetchAnalyzeResult(ranges, false)
	if err != nil {
		return err
	}
	if len(e.idxInfo.Columns) == 1 {
		ranges = ranger.NullRange()
		err = e.fetchAnalyzeResult(ranges, true)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *AnalyzeIndexExec) buildStatsFromResult(result distsql.SelectResult, needCMS bool) (*statistics.Histogram, *statistics.CMSketch, error) {
	hist := &statistics.Histogram{}
	var cms *statistics.CMSketch
	if needCMS {
		cms = statistics.NewCMSketch(defaultCMSketchDepth, defaultCMSketchWidth)
	}
	for {
		data, err := result.NextRaw(context.TODO())
		if err != nil {
			return nil, nil, err
		}
		if data == nil {
			break
		}
		resp := &tipb.AnalyzeIndexResp{}
		err = resp.Unmarshal(data)
		if err != nil {
			return nil, nil, err
		}
		respHist := statistics.HistogramFromProto(resp.Hist)
		e.job.Update(int64(respHist.TotalRowCount()))
		hist, err = statistics.MergeHistograms(e.ctx.GetSessionVars().StmtCtx, hist, respHist, int(e.maxNumBuckets))
		if err != nil {
			return nil, nil, err
		}
		if needCMS {
			if resp.Cms == nil {
				logutil.Logger(context.TODO()).Warn("nil CMS in response", zap.String("table", e.idxInfo.Table.O), zap.String("index", e.idxInfo.Name.O))
			} else {
				if err := cms.MergeCMSketch(statistics.CMSketchFromProto(resp.Cms)); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return hist, cms, nil
}

func (e *AnalyzeIndexExec) buildStats() (hist *statistics.Histogram, cms *statistics.CMSketch, err error) {
	if err = e.open(); err != nil {
		return nil, nil, err
	}
	defer func() {
		err = closeAll(e.result, e.countNullRes)
	}()
	hist, cms, err = e.buildStatsFromResult(e.result, true)
	if err != nil {
		return nil, nil, err
	}
	if e.countNullRes != nil {
		nullHist, _, err := e.buildStatsFromResult(e.countNullRes, false)
		if err != nil {
			return nil, nil, err
		}
		if l := nullHist.Len(); l > 0 {
			hist.NullCount = nullHist.Buckets[l-1].Count
		}
	}
	hist.ID = e.idxInfo.ID
	return hist, cms, nil
}

func analyzeColumnsPushdown(colExec *AnalyzeColumnsExec) analyzeResult {
	hists, cms, err := colExec.buildStats()
	if err != nil {
		return analyzeResult{Err: err, job: colExec.job}
	}
	result := analyzeResult{
		PhysicalTableID: colExec.physicalTableID,
		Hist:            hists,
		Cms:             cms,
		job:             colExec.job,
	}
	hist := hists[0]
	result.Count = hist.NullCount
	if hist.Len() > 0 {
		result.Count += hist.Buckets[hist.Len()-1].Count
	}
	return result
}

// AnalyzeColumnsExec represents Analyze columns push down executor.
type AnalyzeColumnsExec struct {
	ctx             sessionctx.Context
	physicalTableID int64
	colsInfo        []*model.ColumnInfo
	pkInfo          *model.ColumnInfo
	concurrency     int
	priority        int
	analyzePB       *tipb.AnalyzeReq
	resultHandler   *tableResultHandler
	maxNumBuckets   uint64
	job             *statistics.AnalyzeJob
}

func (e *AnalyzeColumnsExec) open() error {
	var ranges []*ranger.Range
	if e.pkInfo != nil {
		ranges = ranger.FullIntRange(mysql.HasUnsignedFlag(e.pkInfo.Flag))
	} else {
		ranges = ranger.FullIntRange(false)
	}
	e.resultHandler = &tableResultHandler{}
	firstPartRanges, secondPartRanges := splitRanges(ranges, true, false)
	firstResult, err := e.buildResp(firstPartRanges)
	if err != nil {
		return err
	}
	if len(secondPartRanges) == 0 {
		e.resultHandler.open(nil, firstResult)
		return nil
	}
	var secondResult distsql.SelectResult
	secondResult, err = e.buildResp(secondPartRanges)
	if err != nil {
		return err
	}
	e.resultHandler.open(firstResult, secondResult)

	return nil
}

func (e *AnalyzeColumnsExec) buildResp(ranges []*ranger.Range) (distsql.SelectResult, error) {
	var builder distsql.RequestBuilder
	// Always set KeepOrder of the request to be true, in order to compute
	// correct `correlation` of columns.
	kvReq, err := builder.SetTableRanges(e.physicalTableID, ranges, nil).
		SetAnalyzeRequest(e.analyzePB).
		SetKeepOrder(true).
		SetConcurrency(e.concurrency).
		Build()
	if err != nil {
		return nil, err
	}
	ctx := context.TODO()
	result, err := distsql.Analyze(ctx, e.ctx.GetClient(), kvReq, e.ctx.GetSessionVars().KVVars, e.ctx.GetSessionVars().InRestrictedSQL)
	if err != nil {
		return nil, err
	}
	result.Fetch(ctx)
	return result, nil
}

func (e *AnalyzeColumnsExec) buildStats() (hists []*statistics.Histogram, cms []*statistics.CMSketch, err error) {
	if err = e.open(); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err1 := e.resultHandler.Close(); err1 != nil {
			hists = nil
			cms = nil
			err = err1
		}
	}()
	pkHist := &statistics.Histogram{}
	collectors := make([]*statistics.SampleCollector, len(e.colsInfo))
	for i := range collectors {
		collectors[i] = &statistics.SampleCollector{
			IsMerger:      true,
			FMSketch:      statistics.NewFMSketch(maxSketchSize),
			MaxSampleSize: maxSampleSize,
			CMSketch:      statistics.NewCMSketch(defaultCMSketchDepth, defaultCMSketchWidth),
		}
	}
	for {
		data, err1 := e.resultHandler.nextRaw(context.TODO())
		if err1 != nil {
			return nil, nil, err1
		}
		if data == nil {
			break
		}
		resp := &tipb.AnalyzeColumnsResp{}
		err = resp.Unmarshal(data)
		if err != nil {
			return nil, nil, err
		}
		sc := e.ctx.GetSessionVars().StmtCtx
		rowCount := int64(0)
		if e.pkInfo != nil {
			respHist := statistics.HistogramFromProto(resp.PkHist)
			rowCount = int64(respHist.TotalRowCount())
			pkHist, err = statistics.MergeHistograms(sc, pkHist, respHist, int(e.maxNumBuckets))
			if err != nil {
				return nil, nil, err
			}
		}
		for i, rc := range resp.Collectors {
			respSample := statistics.SampleCollectorFromProto(rc)
			rowCount = respSample.Count + respSample.NullCount
			collectors[i].MergeSampleCollector(sc, respSample)
		}
		e.job.Update(rowCount)
	}
	timeZone := e.ctx.GetSessionVars().Location()
	if e.pkInfo != nil {
		pkHist.ID = e.pkInfo.ID
		err = pkHist.DecodeTo(&e.pkInfo.FieldType, timeZone)
		if err != nil {
			return nil, nil, err
		}
		hists = append(hists, pkHist)
		cms = append(cms, nil)
	}
	for i, col := range e.colsInfo {
		for j, s := range collectors[i].Samples {
			collectors[i].Samples[j].Ordinal = j
			collectors[i].Samples[j].Value, err = tablecodec.DecodeColumnValue(s.Value.GetBytes(), &col.FieldType, timeZone)
			if err != nil {
				return nil, nil, err
			}
		}
		hg, err := statistics.BuildColumn(e.ctx, int64(e.maxNumBuckets), col.ID, collectors[i], &col.FieldType)
		if err != nil {
			return nil, nil, err
		}
		hists = append(hists, hg)
		cms = append(cms, collectors[i].CMSketch)
	}
	return hists, cms, nil
}

func analyzeFastExec(exec *AnalyzeFastExec) []analyzeResult {
	hists, cms, err := exec.buildStats()
	if err != nil {
		return []analyzeResult{{Err: err}}
	}
	var results []analyzeResult
	hasIdxInfo := len(exec.idxsInfo)
	hasPKInfo := 0
	if exec.pkInfo != nil {
		hasPKInfo = 1
	}
	if hasIdxInfo > 0 {
		for i := hasPKInfo + len(exec.colsInfo); i < len(hists); i++ {
			idxResult := analyzeResult{
				PhysicalTableID: exec.PhysicalTableID,
				Hist:            []*statistics.Histogram{hists[i]},
				Cms:             []*statistics.CMSketch{cms[i]},
				IsIndex:         1,
				Count:           hists[i].NullCount,
			}
			if hists[i].Len() > 0 {
				idxResult.Count += hists[i].Buckets[hists[i].Len()-1].Count
			}
			results = append(results, idxResult)
		}
	}
	hist := hists[0]
	colResult := analyzeResult{
		PhysicalTableID: exec.PhysicalTableID,
		Hist:            hists[:hasPKInfo+len(exec.colsInfo)],
		Cms:             cms[:hasPKInfo+len(exec.colsInfo)],
		Count:           hist.NullCount,
	}
	if hist.Len() > 0 {
		colResult.Count += hist.Buckets[hist.Len()-1].Count
	}
	results = append(results, colResult)
	return results
}

// AnalyzeFastTask is the task for build stats.
type AnalyzeFastTask struct {
	Location  *tikv.KeyLocation
	SampSize  uint64
	LRowCount uint64
	RRowCount uint64
}

// AnalyzeFastExec represents Fast Analyze executor.
type AnalyzeFastExec struct {
	ctx             sessionctx.Context
	PhysicalTableID int64
	pkInfo          *model.ColumnInfo
	colsInfo        []*model.ColumnInfo
	idxsInfo        []*model.IndexInfo
	concurrency     int
	maxNumBuckets   uint64
	table           table.Table
	cache           *tikv.RegionCache
	wg              *sync.WaitGroup
	sampLocs        chan *tikv.KeyLocation
	sampLocRowCount uint64
	tasks           chan *AnalyzeFastTask
	scanTasks       []*tikv.KeyLocation
}

func (e *AnalyzeFastExec) buildStats() (hists []*statistics.Histogram, cms []*statistics.CMSketch, err error) {
	// TODO: do fast analyze.
	return nil, nil, nil
}

// analyzeResult is used to represent analyze result.
type analyzeResult struct {
	// PhysicalTableID is the id of a partition or a table.
	PhysicalTableID int64
	Hist            []*statistics.Histogram
	Cms             []*statistics.CMSketch
	Count           int64
	IsIndex         int
	Err             error
	job             *statistics.AnalyzeJob
}
