package querier

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"

	cortex_worker "github.com/cortexproject/cortex/pkg/querier/worker"
	"github.com/cortexproject/cortex/pkg/util/log"
	"github.com/go-kit/log/level"
	"github.com/google/uuid"
	"github.com/grafana/dskit/ring"
	ring_client "github.com/grafana/dskit/ring/client"
	"github.com/grafana/dskit/services"
	ingester_client "github.com/grafana/tempo/modules/ingester/client"
	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/modules/storage"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/tempopb"
	commonv1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	v1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/validation"
	"github.com/grafana/tempo/tempodb/encoding/common"
	"github.com/grafana/tempo/tempodb/search"
	"github.com/opentracing/opentracing-go"
	ot_log "github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	httpgrpc_server "github.com/weaveworks/common/httpgrpc/server"
	"github.com/weaveworks/common/user"
	"go.uber.org/multierr"
)

var (
	metricIngesterClients = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "tempo",
		Name:      "querier_ingester_clients",
		Help:      "The current number of ingester clients.",
	})
)

const rootSpanNotYetReceivedText = "<root span not yet received>"

// Querier handlers queries.
type Querier struct {
	services.Service

	cfg    Config
	ring   ring.ReadRing
	pool   *ring_client.Pool
	store  storage.Store
	limits *overrides.Overrides

	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher
}

type responseFromIngesters struct {
	addr     string
	response interface{}
}

// New makes a new Querier.
func New(cfg Config, clientCfg ingester_client.Config, ring ring.ReadRing, store storage.Store, limits *overrides.Overrides) (*Querier, error) {
	factory := func(addr string) (ring_client.PoolClient, error) {
		return ingester_client.New(addr, clientCfg)
	}

	q := &Querier{
		cfg:  cfg,
		ring: ring,
		pool: ring_client.NewPool("querier_pool",
			clientCfg.PoolConfig,
			ring_client.NewRingServiceDiscovery(ring),
			factory,
			metricIngesterClients,
			log.Logger),
		store:  store,
		limits: limits,
	}

	q.Service = services.NewBasicService(q.starting, q.running, q.stopping)
	return q, nil
}

func (q *Querier) CreateAndRegisterWorker(handler http.Handler) error {
	q.cfg.Worker.MaxConcurrentRequests = q.cfg.MaxConcurrentQueries
	worker, err := cortex_worker.NewQuerierWorker(
		q.cfg.Worker,
		httpgrpc_server.NewServer(handler),
		log.Logger,
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to create frontend worker: %w", err)
	}

	return q.RegisterSubservices(worker, q.pool)
}

func (q *Querier) RegisterSubservices(s ...services.Service) error {
	var err error
	q.subservices, err = services.NewManager(s...)
	q.subservicesWatcher = services.NewFailureWatcher()
	q.subservicesWatcher.WatchManager(q.subservices)
	return err
}

func (q *Querier) starting(ctx context.Context) error {
	if q.subservices != nil {
		err := services.StartManagerAndAwaitHealthy(ctx, q.subservices)
		if err != nil {
			return fmt.Errorf("failed to start subservices %w", err)
		}
	}

	return nil
}

func (q *Querier) running(ctx context.Context) error {
	if q.subservices != nil {
		select {
		case <-ctx.Done():
			return nil
		case err := <-q.subservicesWatcher.Chan():
			return fmt.Errorf("subservices failed %w", err)
		}
	} else {
		<-ctx.Done()
	}
	return nil
}

func (q *Querier) stopping(_ error) error {
	if q.subservices != nil {
		return services.StopManagerAndAwaitStopped(context.Background(), q.subservices)
	}
	return nil
}

// FindTraceByID implements tempopb.Querier.
func (q *Querier) FindTraceByID(ctx context.Context, req *tempopb.TraceByIDRequest) (*tempopb.TraceByIDResponse, error) {
	if !validation.ValidTraceID(req.TraceID) {
		return nil, fmt.Errorf("invalid trace id")
	}

	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error extracting org id in Querier.FindTraceByID")
	}

	span, ctx := opentracing.StartSpanFromContext(ctx, "Querier.FindTraceByID")
	defer span.Finish()

	var completeTrace *tempopb.Trace
	var spanCount, spanCountTotal, traceCountTotal int
	if req.QueryMode == QueryModeIngesters || req.QueryMode == QueryModeAll {
		replicationSet, err := q.ring.GetReplicationSetForOperation(ring.Read)
		if err != nil {
			return nil, errors.Wrap(err, "error finding ingesters in Querier.FindTraceByID")
		}

		span.LogFields(ot_log.String("msg", "searching ingesters"))
		// get responses from all ingesters in parallel
		responses, err := q.forGivenIngesters(ctx, replicationSet, func(client tempopb.QuerierClient) (interface{}, error) {
			return client.FindTraceByID(opentracing.ContextWithSpan(ctx, span), req)
		})
		if err != nil {
			return nil, errors.Wrap(err, "error querying ingesters in Querier.FindTraceByID")
		}

		for _, r := range responses {
			trace := r.response.(*tempopb.TraceByIDResponse).Trace
			if trace != nil {
				completeTrace, _, _, spanCount = model.CombineTraceProtos(completeTrace, trace)
				spanCountTotal += spanCount
				traceCountTotal++
			}
		}
		span.LogFields(ot_log.String("msg", "done searching ingesters"),
			ot_log.Bool("found", completeTrace != nil),
			ot_log.Int("combinedSpans", spanCountTotal),
			ot_log.Int("combinedTraces", traceCountTotal))
	}

	var failedBlocks int
	if req.QueryMode == QueryModeBlocks || req.QueryMode == QueryModeAll {
		span.LogFields(ot_log.String("msg", "searching store"))
		partialTraces, dataEncodings, blockErrs, err := q.store.Find(opentracing.ContextWithSpan(ctx, span), userID, req.TraceID, req.BlockStart, req.BlockEnd)
		if err != nil {
			return nil, errors.Wrap(err, "error querying store in Querier.FindTraceByID")
		}

		if blockErrs != nil {
			failedBlocks = len(blockErrs)
			_ = level.Warn(log.Logger).Log("msg", fmt.Sprintf("failed to query %d blocks", failedBlocks), "blockErrs", multierr.Combine(blockErrs...))
		}

		span.LogFields(ot_log.String("msg", "done searching store"))

		if len(partialTraces) != 0 {
			traceCountTotal = 0
			spanCountTotal = 0
			// combine partialTraces
			var allBytes []byte
			baseEncoding := dataEncodings[0] // just arbitrarily choose an encoding. generally they will all be the same
			for i, partialTrace := range partialTraces {
				dataEncoding := dataEncodings[i]
				allBytes, _, err = model.CombineTraceBytes(allBytes, partialTrace, baseEncoding, dataEncoding)
				if err != nil {
					return nil, errors.Wrap(err, "error querying store in Querier.FindTraceByID")
				}
			}

			// marshal to proto and add to completeTrace
			storeTrace, err := model.Unmarshal(allBytes, baseEncoding)
			if err != nil {
				return nil, errors.Wrap(err, "error unmarshaling combined trace in Querier.FindTraceByID")
			}

			completeTrace, _, _, spanCount = model.CombineTraceProtos(completeTrace, storeTrace)
			spanCountTotal += spanCount
			traceCountTotal++

			span.LogFields(ot_log.String("msg", "combined trace protos from store"),
				ot_log.Bool("found", completeTrace != nil),
				ot_log.Int("combinedSpans", spanCountTotal),
				ot_log.Int("combinedTraces", traceCountTotal))
		}
	}

	return &tempopb.TraceByIDResponse{
		Trace: completeTrace,
		Metrics: &tempopb.TraceByIDMetrics{
			FailedBlocks: uint32(failedBlocks),
		},
	}, nil
}

// forGivenIngesters runs f, in parallel, for given ingesters
func (q *Querier) forGivenIngesters(ctx context.Context, replicationSet ring.ReplicationSet, f func(client tempopb.QuerierClient) (interface{}, error)) ([]responseFromIngesters, error) {
	results, err := replicationSet.Do(ctx, q.cfg.ExtraQueryDelay, func(ctx context.Context, ingester *ring.InstanceDesc) (interface{}, error) {
		client, err := q.pool.GetClientFor(ingester.Addr)
		if err != nil {
			return nil, err
		}

		resp, err := f(client.(tempopb.QuerierClient))
		if err != nil {
			return nil, err
		}

		return responseFromIngesters{ingester.Addr, resp}, nil
	})
	if err != nil {
		return nil, err
	}

	responses := make([]responseFromIngesters, 0, len(results))
	for _, result := range results {
		responses = append(responses, result.(responseFromIngesters))
	}

	return responses, err
}

func (q *Querier) Search(ctx context.Context, req *tempopb.SearchRequest) (*tempopb.SearchResponse, error) {
	_, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error extracting org id in Querier.Search")
	}

	replicationSet, err := q.ring.GetReplicationSetForOperation(ring.Read)
	if err != nil {
		return nil, errors.Wrap(err, "error finding ingesters in Querier.Search")
	}

	responses, err := q.forGivenIngesters(ctx, replicationSet, func(client tempopb.QuerierClient) (interface{}, error) {
		return client.Search(ctx, req)
	})
	if err != nil {
		return nil, errors.Wrap(err, "error querying ingesters in Querier.Search")
	}

	return q.postProcessSearchResults(req, responses), nil
}

func (q *Querier) SearchTags(ctx context.Context, req *tempopb.SearchTagsRequest) (*tempopb.SearchTagsResponse, error) {
	_, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error extracting org id in Querier.SearchTags")
	}

	replicationSet, err := q.ring.GetReplicationSetForOperation(ring.Read)
	if err != nil {
		return nil, errors.Wrap(err, "error finding ingesters in Querier.SearchTags")
	}

	// Get results from all ingesters
	lookupResults, err := q.forGivenIngesters(ctx, replicationSet, func(client tempopb.QuerierClient) (interface{}, error) {
		return client.SearchTags(ctx, req)
	})
	if err != nil {
		return nil, errors.Wrap(err, "error querying ingesters in Querier.SearchTags")
	}

	// Collect only unique values
	uniqueMap := map[string]struct{}{}
	for _, resp := range lookupResults {
		for _, res := range resp.response.(*tempopb.SearchTagsResponse).TagNames {
			uniqueMap[res] = struct{}{}
		}
	}

	// Extra tags
	for _, k := range search.GetVirtualTags() {
		uniqueMap[k] = struct{}{}
	}

	// Final response (sorted)
	resp := &tempopb.SearchTagsResponse{
		TagNames: make([]string, 0, len(uniqueMap)),
	}
	for k := range uniqueMap {
		resp.TagNames = append(resp.TagNames, k)
	}
	sort.Strings(resp.TagNames)

	return resp, nil
}

func (q *Querier) SearchTagValues(ctx context.Context, req *tempopb.SearchTagValuesRequest) (*tempopb.SearchTagValuesResponse, error) {
	_, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error extracting org id in Querier.SearchTagValues")
	}

	replicationSet, err := q.ring.GetReplicationSetForOperation(ring.Read)
	if err != nil {
		return nil, errors.Wrap(err, "error finding ingesters in Querier.SearchTagValues")
	}

	// Get results from all ingesters
	lookupResults, err := q.forGivenIngesters(ctx, replicationSet, func(client tempopb.QuerierClient) (interface{}, error) {
		return client.SearchTagValues(ctx, req)
	})
	if err != nil {
		return nil, errors.Wrap(err, "error querying ingesters in Querier.SearchTagValues")
	}

	// Collect only unique values
	uniqueMap := map[string]struct{}{}
	for _, resp := range lookupResults {
		for _, res := range resp.response.(*tempopb.SearchTagValuesResponse).TagValues {
			uniqueMap[res] = struct{}{}
		}
	}

	// Extra values
	for _, v := range search.GetVirtualTagValues(req.TagName) {
		uniqueMap[v] = struct{}{}
	}

	// Final response (sorted)
	resp := &tempopb.SearchTagValuesResponse{
		TagValues: make([]string, 0, len(uniqueMap)),
	}
	for k := range uniqueMap {
		resp.TagValues = append(resp.TagValues, k)
	}
	sort.Strings(resp.TagValues)

	return resp, nil
}

// todo(search): consolidate
func (q *Querier) BackendSearch(ctx context.Context, req *tempopb.BackendSearchRequest) (*tempopb.SearchResponse, error) {
	tenantID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error extracting org id in Querier.BackendSearch")
	}

	blockID, err := uuid.FromBytes(req.BlockID)
	if err != nil {
		return nil, err
	}

	var searchErr error
	respMtx := sync.Mutex{}
	resp := &tempopb.SearchResponse{
		Metrics: &tempopb.SearchMetrics{},
	}

	err = q.store.IterateObjects(ctx, tenantID, blockID, int(req.StartPage), int(req.TotalPages), func(id common.ID, obj []byte, dataEncoding string) bool {
		respMtx.Lock()
		resp.Metrics.InspectedTraces++
		resp.Metrics.InspectedBytes += uint64(len(obj))
		respMtx.Unlock()

		start := uint64(math.MaxUint64)
		end := uint64(0)

		trace, err := model.Unmarshal(obj, dataEncoding)
		if err != nil {
			searchErr = err
			return true
		}

		tagFound := false
		if len(req.Search.Tags) == 0 {
			tagFound = true
		}

		var rootSpan *v1.Span
		var rootBatch *v1.ResourceSpans
		// todo: is it possible to shortcircuit this loop?
		for _, b := range trace.Batches {
			if !tagFound && searchAttributes(req.Search.Tags, b.Resource.Attributes) {
				tagFound = true
			}

			for _, ils := range b.InstrumentationLibrarySpans {
				for _, s := range ils.Spans {
					if s.StartTimeUnixNano < start {
						start = s.StartTimeUnixNano
					}
					if s.EndTimeUnixNano > end {
						end = s.EndTimeUnixNano
					}
					if rootSpan == nil && len(s.ParentSpanId) == 0 {
						rootSpan = s
						rootBatch = b
					}

					if tagFound {
						continue
					}

					if searchAttributes(req.Search.Tags, s.Attributes) {
						tagFound = true
					}
				}
			}
		}

		if !tagFound {
			return false
		}

		startMs := start / 1000000
		endMs := end / 1000000
		durationMs := uint32(endMs - startMs)
		if req.Search.MaxDurationMs != 0 && req.Search.MaxDurationMs < durationMs {
			return false
		}
		if req.Search.MinDurationMs != 0 && req.Search.MinDurationMs > durationMs {
			return false
		}
		if uint32(startMs/1000) > req.End || uint32(endMs/1000) < req.Start {
			return false
		}

		// woohoo!
		rootServiceName := rootSpanNotYetReceivedText
		rootSpanName := rootSpanNotYetReceivedText
		if rootSpan != nil && rootBatch != nil {
			rootSpanName = rootSpan.Name

			for _, a := range rootBatch.Resource.Attributes {
				if a.Key == search.ServiceNameTag {
					rootServiceName = a.Value.GetStringValue()
					break
				}
			}
		}

		respMtx.Lock()
		defer respMtx.Unlock()
		resp.Traces = append(resp.Traces, &tempopb.TraceSearchMetadata{
			TraceID:           util.TraceIDToHexString(id),
			RootServiceName:   rootServiceName,
			RootTraceName:     rootSpanName,
			StartTimeUnixNano: start,
			DurationMs:        durationMs,
		})

		return len(resp.Traces) >= int(req.Search.Limit)
	})
	if err != nil {
		return nil, err
	}
	if searchErr != nil {
		return nil, searchErr
	}

	return resp, nil
}

func (q *Querier) postProcessSearchResults(req *tempopb.SearchRequest, rr []responseFromIngesters) *tempopb.SearchResponse {
	response := &tempopb.SearchResponse{
		Metrics: &tempopb.SearchMetrics{},
	}

	traces := map[string]*tempopb.TraceSearchMetadata{}

	for _, r := range rr {
		sr := r.response.(*tempopb.SearchResponse)
		for _, t := range sr.Traces {
			// Just simply take first result for each trace
			if _, ok := traces[t.TraceID]; !ok {
				traces[t.TraceID] = t
			}
		}
		if sr.Metrics != nil {
			response.Metrics.InspectedBytes += sr.Metrics.InspectedBytes
			response.Metrics.InspectedTraces += sr.Metrics.InspectedTraces
			response.Metrics.InspectedBlocks += sr.Metrics.InspectedBlocks
			response.Metrics.SkippedBlocks += sr.Metrics.SkippedBlocks
		}
	}

	for _, t := range traces {
		if t.RootServiceName == "" {
			t.RootServiceName = rootSpanNotYetReceivedText
		}
		response.Traces = append(response.Traces, t)
	}

	// Sort and limit results
	sort.Slice(response.Traces, func(i, j int) bool {
		return response.Traces[i].StartTimeUnixNano > response.Traces[j].StartTimeUnixNano
	})
	if req.Limit != 0 && int(req.Limit) < len(response.Traces) {
		response.Traces = response.Traces[:req.Limit]
	}

	return response
}

// todo: support more attribute types. currently only string is supported
func searchAttributes(tags map[string]string, atts []*commonv1.KeyValue) bool {
	for _, a := range atts {
		var v string
		var ok bool

		if v, ok = tags[a.Key]; !ok {
			continue
		}

		if strings.Contains(a.Value.GetStringValue(), v) {
			return true
		}
	}

	return false
}
