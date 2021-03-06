package netstorage

import (
	"container/heap"
	"errors"
	"flag"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmselect/searchutils"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/decimal"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/VictoriaMetrics/metrics"
)

var (
	maxTagKeysPerSearch          = flag.Int("search.maxTagKeys", 100e3, "The maximum number of tag keys returned from /api/v1/labels")
	maxTagValuesPerSearch        = flag.Int("search.maxTagValues", 100e3, "The maximum number of tag values returned from /api/v1/label/<label_name>/values")
	maxTagValueSuffixesPerSearch = flag.Int("search.maxTagValueSuffixesPerSearch", 100e3, "The maximum number of tag value suffixes returned from /metrics/find")
	maxMetricsPerSearch          = flag.Int("search.maxUniqueTimeseries", 300e3, "The maximum number of unique time series each search can scan")
)

// Result is a single timeseries result.
//
// ProcessSearchQuery returns Result slice.
type Result struct {
	// The name of the metric.
	MetricName storage.MetricName

	// Values are sorted by Timestamps.
	Values     []float64
	Timestamps []int64

	// Marshaled MetricName. Used only for results sorting
	// in app/vmselect/promql
	MetricNameMarshaled []byte
}

func (r *Result) reset() {
	r.MetricName.Reset()
	r.Values = r.Values[:0]
	r.Timestamps = r.Timestamps[:0]
	r.MetricNameMarshaled = r.MetricNameMarshaled[:0]
}

// Results holds results returned from ProcessSearchQuery.
type Results struct {
	tr        storage.TimeRange
	fetchData bool
	deadline  searchutils.Deadline

	packedTimeseries []packedTimeseries
	sr               *storage.Search
}

// Len returns the number of results in rss.
func (rss *Results) Len() int {
	return len(rss.packedTimeseries)
}

// Cancel cancels rss work.
func (rss *Results) Cancel() {
	rss.mustClose()
}

func (rss *Results) mustClose() {
	putStorageSearch(rss.sr)
	rss.sr = nil
}

var timeseriesWorkCh = make(chan *timeseriesWork, gomaxprocs*16)

type timeseriesWork struct {
	rss    *Results
	pts    *packedTimeseries
	f      func(rs *Result, workerID uint)
	doneCh chan error

	rowsProcessed int
}

func init() {
	for i := 0; i < gomaxprocs; i++ {
		go timeseriesWorker(uint(i))
	}
}

func timeseriesWorker(workerID uint) {
	var rs Result
	var rsLastResetTime uint64
	for tsw := range timeseriesWorkCh {
		rss := tsw.rss
		if rss.deadline.Exceeded() {
			tsw.doneCh <- fmt.Errorf("timeout exceeded during query execution: %s", rss.deadline.String())
			continue
		}
		if err := tsw.pts.Unpack(&rs, rss.tr, rss.fetchData); err != nil {
			tsw.doneCh <- fmt.Errorf("error during time series unpacking: %w", err)
			continue
		}
		if len(rs.Timestamps) > 0 || !rss.fetchData {
			tsw.f(&rs, workerID)
		}
		tsw.rowsProcessed = len(rs.Values)
		tsw.doneCh <- nil
		currentTime := fasttime.UnixTimestamp()
		if cap(rs.Values) > 1024*1024 && 4*len(rs.Values) < cap(rs.Values) && currentTime-rsLastResetTime > 10 {
			// Reset rs in order to preseve memory usage after processing big time series with millions of rows.
			rs = Result{}
			rsLastResetTime = currentTime
		}
	}
}

// RunParallel runs f in parallel for all the results from rss.
//
// f shouldn't hold references to rs after returning.
// workerID is the id of the worker goroutine that calls f.
//
// rss becomes unusable after the call to RunParallel.
func (rss *Results) RunParallel(f func(rs *Result, workerID uint)) error {
	defer rss.mustClose()

	// Feed workers with work.
	tsws := make([]*timeseriesWork, len(rss.packedTimeseries))
	for i := range rss.packedTimeseries {
		tsw := &timeseriesWork{
			rss:    rss,
			pts:    &rss.packedTimeseries[i],
			f:      f,
			doneCh: make(chan error, 1),
		}
		timeseriesWorkCh <- tsw
		tsws[i] = tsw
	}
	seriesProcessedTotal := len(rss.packedTimeseries)
	rss.packedTimeseries = rss.packedTimeseries[:0]

	// Wait until work is complete.
	var firstErr error
	rowsProcessedTotal := 0
	for _, tsw := range tsws {
		if err := <-tsw.doneCh; err != nil && firstErr == nil {
			// Return just the first error, since other errors
			// are likely duplicate the first error.
			firstErr = err
		}
		rowsProcessedTotal += tsw.rowsProcessed
	}

	perQueryRowsProcessed.Update(float64(rowsProcessedTotal))
	perQuerySeriesProcessed.Update(float64(seriesProcessedTotal))
	return firstErr
}

var perQueryRowsProcessed = metrics.NewHistogram(`vm_per_query_rows_processed_count`)
var perQuerySeriesProcessed = metrics.NewHistogram(`vm_per_query_series_processed_count`)

var gomaxprocs = runtime.GOMAXPROCS(-1)

type packedTimeseries struct {
	metricName string
	brs        []storage.BlockRef
}

var unpackWorkCh = make(chan *unpackWork, gomaxprocs*128)

type unpackWorkItem struct {
	br storage.BlockRef
	tr storage.TimeRange
}

type unpackWork struct {
	ws        []unpackWorkItem
	fetchData bool
	sbs       []*sortBlock
	doneCh    chan error
}

func (upw *unpackWork) reset() {
	ws := upw.ws
	for i := range ws {
		w := &ws[i]
		w.br = storage.BlockRef{}
		w.tr = storage.TimeRange{}
	}
	upw.ws = upw.ws[:0]
	upw.fetchData = false
	sbs := upw.sbs
	for i := range sbs {
		sbs[i] = nil
	}
	upw.sbs = upw.sbs[:0]
	if n := len(upw.doneCh); n > 0 {
		logger.Panicf("BUG: upw.doneCh must be empty; it contains %d items now", n)
	}
}

func (upw *unpackWork) unpack() {
	for _, w := range upw.ws {
		sb := getSortBlock()
		if err := sb.unpackFrom(w.br, w.tr, upw.fetchData); err != nil {
			putSortBlock(sb)
			upw.doneCh <- fmt.Errorf("cannot unpack block: %w", err)
			return
		}
		upw.sbs = append(upw.sbs, sb)
	}
	upw.doneCh <- nil
}

func getUnpackWork() *unpackWork {
	v := unpackWorkPool.Get()
	if v != nil {
		return v.(*unpackWork)
	}
	return &unpackWork{
		doneCh: make(chan error, 1),
	}
}

func putUnpackWork(upw *unpackWork) {
	upw.reset()
	unpackWorkPool.Put(upw)
}

var unpackWorkPool sync.Pool

func init() {
	for i := 0; i < gomaxprocs; i++ {
		go unpackWorker()
	}
}

func unpackWorker() {
	for upw := range unpackWorkCh {
		upw.unpack()
	}
}

// unpackBatchSize is the maximum number of blocks that may be unpacked at once by a single goroutine.
//
// This batch is needed in order to reduce contention for upackWorkCh in multi-CPU system.
var unpackBatchSize = 8 * runtime.GOMAXPROCS(-1)

// Unpack unpacks pts to dst.
func (pts *packedTimeseries) Unpack(dst *Result, tr storage.TimeRange, fetchData bool) error {
	dst.reset()

	if err := dst.MetricName.Unmarshal(bytesutil.ToUnsafeBytes(pts.metricName)); err != nil {
		return fmt.Errorf("cannot unmarshal metricName %q: %w", pts.metricName, err)
	}

	// Feed workers with work
	upws := make([]*unpackWork, 0, 1+len(pts.brs)/unpackBatchSize)
	upw := getUnpackWork()
	upw.fetchData = fetchData
	for _, br := range pts.brs {
		if len(upw.ws) >= unpackBatchSize {
			unpackWorkCh <- upw
			upws = append(upws, upw)
			upw = getUnpackWork()
			upw.fetchData = fetchData
		}
		upw.ws = append(upw.ws, unpackWorkItem{
			br: br,
			tr: tr,
		})
	}
	unpackWorkCh <- upw
	upws = append(upws, upw)
	pts.brs = pts.brs[:0]

	// Wait until work is complete
	sbs := make([]*sortBlock, 0, len(pts.brs))
	var firstErr error
	for _, upw := range upws {
		if err := <-upw.doneCh; err != nil && firstErr == nil {
			// Return the first error only, since other errors are likely the same.
			firstErr = err
		}
		if firstErr == nil {
			sbs = append(sbs, upw.sbs...)
		} else {
			for _, sb := range upw.sbs {
				putSortBlock(sb)
			}
		}
		putUnpackWork(upw)
	}
	if firstErr != nil {
		return firstErr
	}
	mergeSortBlocks(dst, sbs)
	return nil
}

func getSortBlock() *sortBlock {
	v := sbPool.Get()
	if v == nil {
		return &sortBlock{}
	}
	return v.(*sortBlock)
}

func putSortBlock(sb *sortBlock) {
	sb.reset()
	sbPool.Put(sb)
}

var sbPool sync.Pool

var metricRowsSkipped = metrics.NewCounter(`vm_metric_rows_skipped_total{name="vmselect"}`)

func mergeSortBlocks(dst *Result, sbh sortBlocksHeap) {
	// Skip empty sort blocks, since they cannot be passed to heap.Init.
	src := sbh
	sbh = sbh[:0]
	for _, sb := range src {
		if len(sb.Timestamps) == 0 {
			putSortBlock(sb)
			continue
		}
		sbh = append(sbh, sb)
	}
	if len(sbh) == 0 {
		return
	}
	heap.Init(&sbh)
	for {
		top := sbh[0]
		heap.Pop(&sbh)
		if len(sbh) == 0 {
			dst.Timestamps = append(dst.Timestamps, top.Timestamps[top.NextIdx:]...)
			dst.Values = append(dst.Values, top.Values[top.NextIdx:]...)
			putSortBlock(top)
			break
		}
		sbNext := sbh[0]
		tsNext := sbNext.Timestamps[sbNext.NextIdx]
		idxNext := len(top.Timestamps)
		if top.Timestamps[idxNext-1] > tsNext {
			idxNext = top.NextIdx
			for top.Timestamps[idxNext] <= tsNext {
				idxNext++
			}
		}
		dst.Timestamps = append(dst.Timestamps, top.Timestamps[top.NextIdx:idxNext]...)
		dst.Values = append(dst.Values, top.Values[top.NextIdx:idxNext]...)
		if idxNext < len(top.Timestamps) {
			top.NextIdx = idxNext
			heap.Push(&sbh, top)
		} else {
			// Return top to the pool.
			putSortBlock(top)
		}
	}

	timestamps, values := storage.DeduplicateSamples(dst.Timestamps, dst.Values)
	dedups := len(dst.Timestamps) - len(timestamps)
	dedupsDuringSelect.Add(dedups)
	dst.Timestamps = timestamps
	dst.Values = values
}

var dedupsDuringSelect = metrics.NewCounter(`vm_deduplicated_samples_total{type="select"}`)

type sortBlock struct {
	// b is used as a temporary storage for unpacked rows before they
	// go to Timestamps and Values.
	b storage.Block

	Timestamps []int64
	Values     []float64
	NextIdx    int
}

func (sb *sortBlock) reset() {
	sb.b.Reset()
	sb.Timestamps = sb.Timestamps[:0]
	sb.Values = sb.Values[:0]
	sb.NextIdx = 0
}

func (sb *sortBlock) unpackFrom(br storage.BlockRef, tr storage.TimeRange, fetchData bool) error {
	br.MustReadBlock(&sb.b, fetchData)
	if fetchData {
		if err := sb.b.UnmarshalData(); err != nil {
			return fmt.Errorf("cannot unmarshal block: %w", err)
		}
	}
	timestamps := sb.b.Timestamps()

	// Skip timestamps smaller than tr.MinTimestamp.
	i := 0
	for i < len(timestamps) && timestamps[i] < tr.MinTimestamp {
		i++
	}

	// Skip timestamps bigger than tr.MaxTimestamp.
	j := len(timestamps)
	for j > i && timestamps[j-1] > tr.MaxTimestamp {
		j--
	}
	skippedRows := sb.b.RowsCount() - (j - i)
	metricRowsSkipped.Add(skippedRows)

	// Copy the remaining values.
	if i == j {
		return nil
	}
	values := sb.b.Values()
	sb.Timestamps = append(sb.Timestamps, timestamps[i:j]...)
	sb.Values = decimal.AppendDecimalToFloat(sb.Values, values[i:j], sb.b.Scale())
	return nil
}

type sortBlocksHeap []*sortBlock

func (sbh sortBlocksHeap) Len() int {
	return len(sbh)
}

func (sbh sortBlocksHeap) Less(i, j int) bool {
	a := sbh[i]
	b := sbh[j]
	return a.Timestamps[a.NextIdx] < b.Timestamps[b.NextIdx]
}

func (sbh sortBlocksHeap) Swap(i, j int) {
	sbh[i], sbh[j] = sbh[j], sbh[i]
}

func (sbh *sortBlocksHeap) Push(x interface{}) {
	*sbh = append(*sbh, x.(*sortBlock))
}

func (sbh *sortBlocksHeap) Pop() interface{} {
	a := *sbh
	v := a[len(a)-1]
	*sbh = a[:len(a)-1]
	return v
}

// DeleteSeries deletes time series matching the given tagFilterss.
func DeleteSeries(sq *storage.SearchQuery) (int, error) {
	tfss, err := setupTfss(sq.TagFilterss)
	if err != nil {
		return 0, err
	}
	return vmstorage.DeleteMetrics(tfss)
}

// GetLabels returns labels until the given deadline.
func GetLabels(deadline searchutils.Deadline) ([]string, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	labels, err := vmstorage.SearchTagKeys(*maxTagKeysPerSearch, deadline.Deadline())
	if err != nil {
		return nil, fmt.Errorf("error during labels search: %w", err)
	}

	// Substitute "" with "__name__"
	for i := range labels {
		if labels[i] == "" {
			labels[i] = "__name__"
		}
	}

	// Sort labels like Prometheus does
	sort.Strings(labels)

	return labels, nil
}

// GetLabelValues returns label values for the given labelName
// until the given deadline.
func GetLabelValues(labelName string, deadline searchutils.Deadline) ([]string, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	if labelName == "__name__" {
		labelName = ""
	}

	// Search for tag values
	labelValues, err := vmstorage.SearchTagValues([]byte(labelName), *maxTagValuesPerSearch, deadline.Deadline())
	if err != nil {
		return nil, fmt.Errorf("error during label values search for labelName=%q: %w", labelName, err)
	}

	// Sort labelValues like Prometheus does
	sort.Strings(labelValues)

	return labelValues, nil
}

// GetTagValueSuffixes returns tag value suffixes for the given tagKey and the given tagValuePrefix.
//
// It can be used for implementing https://graphite-api.readthedocs.io/en/latest/api.html#metrics-find
func GetTagValueSuffixes(tr storage.TimeRange, tagKey, tagValuePrefix string, delimiter byte, deadline searchutils.Deadline) ([]string, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	suffixes, err := vmstorage.SearchTagValueSuffixes(tr, []byte(tagKey), []byte(tagValuePrefix), delimiter, *maxTagValueSuffixesPerSearch, deadline.Deadline())
	if err != nil {
		return nil, fmt.Errorf("error during search for suffixes for tagKey=%q, tagValuePrefix=%q, delimiter=%c on time range %s: %w",
			tagKey, tagValuePrefix, delimiter, tr.String(), err)
	}
	return suffixes, nil
}

// GetLabelEntries returns all the label entries until the given deadline.
func GetLabelEntries(deadline searchutils.Deadline) ([]storage.TagEntry, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	labelEntries, err := vmstorage.SearchTagEntries(*maxTagKeysPerSearch, *maxTagValuesPerSearch, deadline.Deadline())
	if err != nil {
		return nil, fmt.Errorf("error during label entries request: %w", err)
	}

	// Substitute "" with "__name__"
	for i := range labelEntries {
		e := &labelEntries[i]
		if e.Key == "" {
			e.Key = "__name__"
		}
	}

	// Sort labelEntries by the number of label values in each entry.
	sort.Slice(labelEntries, func(i, j int) bool {
		a, b := labelEntries[i].Values, labelEntries[j].Values
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return labelEntries[i].Key > labelEntries[j].Key
	})

	return labelEntries, nil
}

// GetTSDBStatusForDate returns tsdb status according to https://prometheus.io/docs/prometheus/latest/querying/api/#tsdb-stats
func GetTSDBStatusForDate(deadline searchutils.Deadline, date uint64, topN int) (*storage.TSDBStatus, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	status, err := vmstorage.GetTSDBStatusForDate(date, topN, deadline.Deadline())
	if err != nil {
		return nil, fmt.Errorf("error during tsdb status request: %w", err)
	}
	return status, nil
}

// GetSeriesCount returns the number of unique series.
func GetSeriesCount(deadline searchutils.Deadline) (uint64, error) {
	if deadline.Exceeded() {
		return 0, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}
	n, err := vmstorage.GetSeriesCount(deadline.Deadline())
	if err != nil {
		return 0, fmt.Errorf("error during series count request: %w", err)
	}
	return n, nil
}

func getStorageSearch() *storage.Search {
	v := ssPool.Get()
	if v == nil {
		return &storage.Search{}
	}
	return v.(*storage.Search)
}

func putStorageSearch(sr *storage.Search) {
	sr.MustClose()
	ssPool.Put(sr)
}

var ssPool sync.Pool

// ProcessSearchQuery performs sq on storage nodes until the given deadline.
//
// Results.RunParallel or Results.Cancel must be called on the returned Results.
func ProcessSearchQuery(sq *storage.SearchQuery, fetchData bool, deadline searchutils.Deadline) (*Results, error) {
	if deadline.Exceeded() {
		return nil, fmt.Errorf("timeout exceeded before starting the query processing: %s", deadline.String())
	}

	// Setup search.
	tfss, err := setupTfss(sq.TagFilterss)
	if err != nil {
		return nil, err
	}
	tr := storage.TimeRange{
		MinTimestamp: sq.MinTimestamp,
		MaxTimestamp: sq.MaxTimestamp,
	}
	if err := vmstorage.CheckTimeRange(tr); err != nil {
		return nil, err
	}

	vmstorage.WG.Add(1)
	defer vmstorage.WG.Done()

	sr := getStorageSearch()
	maxSeriesCount := sr.Init(vmstorage.Storage, tfss, tr, *maxMetricsPerSearch, deadline.Deadline())

	m := make(map[string][]storage.BlockRef, maxSeriesCount)
	orderedMetricNames := make([]string, 0, maxSeriesCount)
	blocksRead := 0
	for sr.NextMetricBlock() {
		blocksRead++
		if deadline.Exceeded() {
			return nil, fmt.Errorf("timeout exceeded while fetching data block #%d from storage: %s", blocksRead, deadline.String())
		}
		metricName := sr.MetricBlockRef.MetricName
		brs := m[string(metricName)]
		brs = append(brs, *sr.MetricBlockRef.BlockRef)
		if len(brs) > 1 {
			// An optimization: do not allocate a string for already existing metricName key in m
			m[string(metricName)] = brs
		} else {
			// An optimization for big number of time series with long metricName values:
			// use only a single copy of metricName for both orderedMetricNames and m.
			orderedMetricNames = append(orderedMetricNames, string(metricName))
			m[orderedMetricNames[len(orderedMetricNames)-1]] = brs
		}
	}
	if err := sr.Error(); err != nil {
		if errors.Is(err, storage.ErrDeadlineExceeded) {
			return nil, fmt.Errorf("timeout exceeded during the query: %s", deadline.String())
		}
		return nil, fmt.Errorf("search error after reading %d data blocks: %w", blocksRead, err)
	}

	var rss Results
	rss.tr = tr
	rss.fetchData = fetchData
	rss.deadline = deadline
	pts := make([]packedTimeseries, len(orderedMetricNames))
	for i, metricName := range orderedMetricNames {
		pts[i] = packedTimeseries{
			metricName: metricName,
			brs:        m[metricName],
		}
	}
	rss.packedTimeseries = pts
	rss.sr = sr
	return &rss, nil
}

func setupTfss(tagFilterss [][]storage.TagFilter) ([]*storage.TagFilters, error) {
	tfss := make([]*storage.TagFilters, 0, len(tagFilterss))
	for _, tagFilters := range tagFilterss {
		tfs := storage.NewTagFilters()
		for i := range tagFilters {
			tf := &tagFilters[i]
			if err := tfs.Add(tf.Key, tf.Value, tf.IsNegative, tf.IsRegexp); err != nil {
				return nil, fmt.Errorf("cannot parse tag filter %s: %w", tf, err)
			}
		}
		tfss = append(tfss, tfs)
		tfss = append(tfss, tfs.Finalize()...)
	}
	return tfss, nil
}
