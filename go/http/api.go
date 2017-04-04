package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/github/freno/go/base"
	"github.com/github/freno/go/group"
	"github.com/github/freno/go/throttle"
	metrics "github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/exp"

	"github.com/julienschmidt/httprouter"
)

// API exposes the contract for the throttler's web API
type API interface {
	LbCheck(w http.ResponseWriter, _ *http.Request, _ httprouter.Params)
	LeaderCheck(w http.ResponseWriter, _ *http.Request, _ httprouter.Params)
	RaftLeader(w http.ResponseWriter, _ *http.Request, _ httprouter.Params)
	RaftState(w http.ResponseWriter, _ *http.Request, _ httprouter.Params)
	Hostname(w http.ResponseWriter, _ *http.Request, _ httprouter.Params)
	CheckMySQLCluster(w http.ResponseWriter, r *http.Request, _ httprouter.Params)
	AggregatedMetrics(w http.ResponseWriter, r *http.Request, _ httprouter.Params)
	ThrottleApp(w http.ResponseWriter, r *http.Request, _ httprouter.Params)
	UnthrottleApp(w http.ResponseWriter, r *http.Request, _ httprouter.Params)
}

type CheckResponse struct {
	StatusCode int
	Message    string
	Value      float64
	Threshold  float64
}

func NewCheckResponse(statusCode int, err error, value float64, threshold float64) *CheckResponse {
	response := &CheckResponse{
		StatusCode: statusCode,
		Value:      value,
		Threshold:  threshold,
	}
	if err != nil {
		response.Message = err.Error()
	}
	return response
}

type GeneralResponse struct {
	StatusCode int
	Message    string
}

func NewGeneralResponse(statusCode int, message string) *GeneralResponse {
	return &GeneralResponse{StatusCode: statusCode, Message: message}
}

// APIImpl implements the API
type APIImpl struct {
	throttler        *throttle.Throttler
	consensusService group.ConsensusService
	hostname         string
}

// NewAPIImpl creates a new instance of the API implementation
func NewAPIImpl(throttler *throttle.Throttler, consensusService group.ConsensusService) *APIImpl {
	api := &APIImpl{
		throttler:        throttler,
		consensusService: consensusService,
	}
	if hostname, err := os.Hostname(); err == nil {
		api.hostname = hostname
	}
	return api
}

// respondGeneric will generate a generic response in the form of {status, message}
// It will set response based on whether request is HEAD/GET and based on given error
func (api *APIImpl) respondGeneric(w http.ResponseWriter, r *http.Request, e error) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
	}
	var generalRespnse *GeneralResponse
	if e == nil {
		generalRespnse = NewGeneralResponse(http.StatusOK, "OK")
	} else {
		generalRespnse = NewGeneralResponse(http.StatusInternalServerError, e.Error())
	}
	w.WriteHeader(generalRespnse.StatusCode)
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(generalRespnse)
	}
}

// LbCheck responds to LbCheck with HTTP 200
func (api *APIImpl) LbCheck(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	api.respondGeneric(w, r, nil)
}

// LeaderCheck responds with HTTP 200 when this node is a raft leader, otherwise 404
// This is a useful check for HAProxy routing
func (api *APIImpl) LeaderCheck(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	statusCode := http.StatusNotFound
	if group.IsLeader() {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)
	if r.Method == http.MethodGet {
		fmt.Fprintf(w, "HTTP %d", statusCode)
	}
}

// RaftLeader returns the identity of the leader
func (api *APIImpl) RaftLeader(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	if leader := group.GetLeader(); leader != "" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, leader)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// RaftState returns the state of the raft node
func (api *APIImpl) RaftState(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	fmt.Fprintf(w, group.GetState().String())
}

// Hostname returns the hostname this process executes on
func (api *APIImpl) Hostname(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	if api.hostname != "" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, api.hostname)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (api *APIImpl) checkAppMetricResult(w http.ResponseWriter, r *http.Request, ps httprouter.Params, metricResultFunc base.MetricResultFunc) {
	appName := ps.ByName("app")
	metricResult, threshold := api.throttler.AppRequestMetricResult(appName, metricResultFunc)
	value, err := metricResult.Get()

	statusCode := http.StatusInternalServerError // 500

	defer func(appName string, statusCode *int) {
		go func() {
			metrics.GetOrRegisterCounter("check.any.total", nil).Inc(1)
			metrics.GetOrRegisterCounter(fmt.Sprintf("check.%s.total", appName), nil).Inc(1)
			if *statusCode != http.StatusOK {
				metrics.GetOrRegisterCounter("check.any.error", nil).Inc(1)
				metrics.GetOrRegisterCounter(fmt.Sprintf("check.%s.error", appName), nil).Inc(1)
			}
		}()
	}(appName, &statusCode)

	if err == base.AppDeniedError {
		// app specifically not allowed to get metrics
		statusCode = http.StatusExpectationFailed // 417
	} else if err == base.NoSuchMetricError {
		// not collected yet, or metric does not exist
		statusCode = http.StatusNotFound // 404
	} else if err != nil {
		// any error
		statusCode = http.StatusInternalServerError // 500
	} else if value > threshold {
		// casual throttling
		statusCode = http.StatusTooManyRequests // 429
		err = base.ThresholdExceededError
	} else {
		// all good!
		statusCode = http.StatusOK // 200
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(statusCode)
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(NewCheckResponse(statusCode, err, value, threshold))
	}
}

// CheckMySQLCluster checks whether a cluster's collected metric is within its threshold
func (api *APIImpl) CheckMySQLCluster(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	clusterName := ps.ByName("clusterName")
	var metricResultFunc base.MetricResultFunc = func() (metricResult base.MetricResult, threshold float64) {
		return api.throttler.GetMySQLClusterMetrics(clusterName)
	}
	api.checkAppMetricResult(w, r, ps, metricResultFunc)
}

// AggregatedMetrics returns a snapshot of all current aggregated metrics
func (api *APIImpl) AggregatedMetrics(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.Header().Set("Content-Type", "application/json")
	aggregatedMetrics := api.throttler.AggregatedMetrics()
	responseMap := map[string]string{}
	for metricName, metric := range aggregatedMetrics {
		value, err := metric.Get()
		responseMap[metricName] = fmt.Sprintf("%+v, %+v", value, err)
	}
	json.NewEncoder(w).Encode(responseMap)
}

// ThrottleApp forcibly marks given app as throttled. Future requests by this app will be denied.
func (api *APIImpl) ThrottleApp(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	appName := ps.ByName("app")
	err := api.consensusService.ThrottleApp(appName)

	api.respondGeneric(w, r, err)
}

// ThrottleApp unthrottles given app.
func (api *APIImpl) UnthrottleApp(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	appName := ps.ByName("app")
	err := api.consensusService.UnthrottleApp(appName)

	api.respondGeneric(w, r, err)
}

// register is a wrapper function for accepting both GET and HEAD requests
func register(router *httprouter.Router, path string, f httprouter.Handle) {
	router.HEAD(path, f)
	router.GET(path, f)
}

func metricsHandle(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	handler := exp.ExpHandler(metrics.DefaultRegistry)
	handler.ServeHTTP(w, r)
}

// ConfigureRoutes configures a set of HTTP routes to be actions dispatched by the
// given api's methods.
func ConfigureRoutes(api API) *httprouter.Router {
	router := httprouter.New()
	register(router, "/lb-check", api.LbCheck)
	register(router, "/_ping", api.LbCheck)
	register(router, "/status", api.LbCheck)
	register(router, "/leader-check", api.LeaderCheck)
	register(router, "/raft/leader", api.RaftLeader)
	register(router, "/raft/state", api.RaftState)
	register(router, "/hostname", api.Hostname)
	register(router, "/check/:app/mysql/:clusterName", api.CheckMySQLCluster)
	register(router, "/aggregated-metrics", api.AggregatedMetrics)
	register(router, "/throttle-app/:app", api.ThrottleApp)
	register(router, "/unthrottle-app/:app", api.UnthrottleApp)

	router.GET("/debug/vars", metricsHandle)
	router.GET("/debug/metrics", metricsHandle)

	return router
}