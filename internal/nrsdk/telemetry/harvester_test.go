package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/newrelic/newrelic-istio-adapter/internal/nrsdk/internal"
)

// compactJSONString removes the whitespace from a JSON string.  This function
// will panic if the string provided is not valid JSON.
func compactJSONString(js string) string {
	buf := new(bytes.Buffer)
	if err := json.Compact(buf, []byte(js)); err != nil {
		panic(fmt.Errorf("unable to compact JSON: %v", err))
	}
	return buf.String()
}

func TestNilHarvestNow(t *testing.T) {
	var h *Harvester
	h.HarvestNow(context.Background())
}

func TestNilHarvesterRecordSpan(t *testing.T) {
	var h *Harvester
	h.RecordSpan(Span{
		ID:          "id",
		TraceID:     "traceId",
		Name:        "myspan",
		ParentID:    "parentId",
		Timestamp:   time.Now(),
		Duration:    time.Second,
		ServiceName: "span",
		Attributes: map[string]interface{}{
			"attr": 1,
		},
	})
}

func TestHarvestErrorLogger(t *testing.T) {
	err := map[string]interface{}{}

	harvestMissingErrorLogger := NewHarvester()
	harvestMissingErrorLogger.config.logError(err)

	var savedErrors []map[string]interface{}
	h := NewHarvester(func(cfg *Config) {
		cfg.ErrorLogger = func(e map[string]interface{}) {
			savedErrors = append(savedErrors, e)
		}
	})
	h.config.logError(err)
	if len(savedErrors) != 1 {
		t.Error("incorrect errors found", savedErrors)
	}
}

func TestHarvestDebugLogger(t *testing.T) {
	fields := map[string]interface{}{
		"something": "happened",
	}

	emptyHarvest := NewHarvester()
	emptyHarvest.config.logDebug(fields)

	var savedFields map[string]interface{}
	h := NewHarvester(func(cfg *Config) {
		cfg.DebugLogger = func(f map[string]interface{}) {
			savedFields = f
		}
	})
	h.config.logDebug(fields)
	if !reflect.DeepEqual(fields, savedFields) {
		t.Error(fields, savedFields)
	}
}

func TestVetCommonAttributes(t *testing.T) {
	attributes := map[string]interface{}{
		"bool":           true,
		"bad":            struct{}{},
		"int":            123,
		"remove-me":      t,
		"nil-is-invalid": nil,
	}
	var savedErrors []map[string]interface{}
	NewHarvester(
		ConfigCommonAttributes(attributes),
		func(cfg *Config) {
			cfg.ErrorLogger = func(e map[string]interface{}) {
				savedErrors = append(savedErrors, e)
			}
		},
	)
	if len(savedErrors) != 3 {
		t.Fatal(savedErrors)
	}
}

func TestNeedsHarvestThread(t *testing.T) {
	testcases := []struct {
		cfgfn              func(cfg *Config)
		needsHarvestThread bool
	}{
		{
			cfgfn:              func(cfg *Config) {},
			needsHarvestThread: false,
		},
		{
			cfgfn: func(cfg *Config) {
				cfg.HarvestPeriod = 5 * time.Second
				cfg.APIKey = "APIKey"
			},
			needsHarvestThread: true,
		},
		{
			cfgfn: func(cfg *Config) {
				cfg.HarvestPeriod = 0
				cfg.APIKey = "APIKey"
			},
			needsHarvestThread: false,
		},
		{
			cfgfn: func(cfg *Config) {
				cfg.HarvestPeriod = 5 * time.Second
				cfg.APIKey = ""
			},
			needsHarvestThread: false,
		},
		{
			cfgfn: func(cfg *Config) {
				cfg.HarvestPeriod = 0
				cfg.APIKey = ""
			},
			needsHarvestThread: false,
		},
	}
	for idx, tc := range testcases {
		h := NewHarvester(tc.cfgfn)
		got := h.needsHarvestThread()
		if got != tc.needsHarvestThread {
			t.Error(idx, got, tc.needsHarvestThread)
		}
	}
}

func TestHarvestCancelled(t *testing.T) {
	var errs int
	var posts int
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// Test that the context with the deadline is added to the
		// harvest request.
		<-r.Context().Done()
		posts++
		return emptyResponse(500), nil
	})
	h := NewHarvester(func(cfg *Config) {
		cfg.ErrorLogger = func(e map[string]interface{}) {
			errs++
		}
		cfg.HarvestPeriod = 0
		cfg.Client.Transport = rt
		cfg.APIKey = "key"
		cfg.RetryBackoff = time.Hour
	})
	h.RecordSpan(Span{TraceID: "id", ID: "id"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.HarvestNow(ctx)

	if posts != 1 {
		t.Error("incorrect number of tries tried", posts)
	}
	if errs != 2 {
		t.Error("incorrect number of errors logged", errs)
	}
}

func TestNewRequestHeaders(t *testing.T) {
	h := NewHarvester(configTesting)
	h.RecordSpan(Span{TraceID: "id", ID: "id"})
	h.RecordMetric(Gauge{})

	reqs := h.swapOutSpans()
	if len(reqs) != 1 {
		t.Fatal(reqs)
	}
	req := reqs[0]
	if h := req.Request.Header.Get("Content-Encoding"); "gzip" != h {
		t.Error("incorrect Content-Encoding header", req.Request.Header)
	}
	if h := req.Request.Header.Get("User-Agent"); "" == h {
		t.Error("User-Agent header not found", req.Request.Header)
	}

	reqs = h.swapOutMetrics(time.Now())
	if len(reqs) != 1 {
		t.Fatal(reqs)
	}
	req = reqs[0]
	if h := req.Request.Header.Get("Content-Type"); "application/json" != h {
		t.Error("incorrect Content-Type", h)
	}
	if h := req.Request.Header.Get("Api-Key"); "api-key" != h {
		t.Error("incorrect Api-Key", h)
	}
	if h := req.Request.Header.Get("Content-Encoding"); "gzip" != h {
		t.Error("incorrect Content-Encoding header", h)
	}
	if h := req.Request.Header.Get("User-Agent"); "" == h {
		t.Error("User-Agent header not found", h)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// optional interface required for go1.4 and go1.5
func (fn roundTripperFunc) CancelRequest(*http.Request) {}

func emptyResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
}

func uncompressBody(req *http.Request) (string, error) {
	body, err := ioutil.ReadAll(req.Body)
	defer req.Body.Close()

	if err != nil {
		return "", fmt.Errorf("unable to read body: %v", err)
	}
	uncompressed, err := internal.Uncompress(body)
	if err != nil {
		return "", fmt.Errorf("unable to uncompress body: %v", err)
	}
	return string(uncompressed), nil
}

// sortedMetricsHelper is used to sort metrics for JSON comparison.
type sortedMetricsHelper []json.RawMessage

func (h sortedMetricsHelper) Len() int {
	return len(h)
}
func (h sortedMetricsHelper) Less(i, j int) bool {
	return string(h[i]) < string(h[j])
}
func (h sortedMetricsHelper) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func testHarvesterMetrics(t testing.TB, h *Harvester, expect string) {
	reqs := h.swapOutMetrics(time.Now())
	if len(reqs) != 1 {
		t.Fatal(reqs)
	}
	js := reqs[0].UncompressedBody
	var helper []struct {
		Metrics sortedMetricsHelper `json:"metrics"`
	}
	if err := json.Unmarshal(js, &helper); err != nil {
		t.Fatal("unable to unmarshal metrics for sorting", err)
		return
	}
	sort.Sort(helper[0].Metrics)
	js, err := json.Marshal(helper[0].Metrics)
	if nil != err {
		t.Fatal("unable to marshal metrics", err)
		return
	}
	actual := string(js)

	if th, ok := t.(interface{ Helper() }); ok {
		th.Helper()
	}
	compactExpect := compactJSONString(expect)
	if compactExpect != actual {
		t.Errorf("\nexpect=%s\nactual=%s\n", compactExpect, actual)
	}
}

func TestRecordMetric(t *testing.T) {
	start := time.Date(2014, time.November, 28, 1, 1, 0, 0, time.UTC)
	h := NewHarvester(configTesting)
	h.RecordMetric(Count{
		Name:           "myCount",
		AttributesJSON: json.RawMessage(`{"zip":"zap"}`),
		Value:          123,
		Timestamp:      start,
		Interval:       5 * time.Second,
	})
	h.RecordMetric(Gauge{
		Name:       "myGauge",
		Attributes: map[string]interface{}{"zippity": "zappity"},
		Value:      246,
		Timestamp:  start,
	})
	h.RecordMetric(Summary{
		Name:       "mySummary",
		Attributes: map[string]interface{}{"zup": "zop"},
		Count:      3,
		Sum:        15,
		Min:        4,
		Max:        6,
		Timestamp:  start,
		Interval:   5 * time.Second,
	})
	expect := `[
		{"name":"myCount","type":"count","value":123,"timestamp":1417136460000,"interval.ms":5000,"attributes":{"zip":"zap"}},
		{"name":"myGauge","type":"gauge","value":246,"timestamp":1417136460000,"attributes":{"zippity":"zappity"}},
		{"name":"mySummary","type":"summary","value":{"sum":15,"count":3,"min":4,"max":6},"timestamp":1417136460000,"interval.ms":5000,"attributes":{"zup":"zop"}}
	]`
	testHarvesterMetrics(t, h, expect)
}

func TestReturnCodes(t *testing.T) {
	// tests which return codes should retry and which should not
	testcases := []struct {
		returnCode  int
		shouldRetry bool
	}{
		{200, false},
		{202, false},
		{400, false},
		{403, false},
		{404, false},
		{405, false},
		{411, false},
		{413, false},
		{429, true},
		{500, true},
		{503, true},
	}

	var posts int
	sp := Span{TraceID: "id", ID: "id", Name: "span1", Timestamp: time.Date(2014, time.November, 28, 1, 1, 0, 0, time.UTC)}

	rtFunc := func(code int) roundTripperFunc {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			posts++
			if posts > 1 {
				return emptyResponse(202), nil
			}
			return emptyResponse(code), nil
		})
	}

	for _, test := range testcases {
		posts = 0
		h := NewHarvester(configTesting, func(cfg *Config) {
			cfg.Client.Transport = rtFunc(test.returnCode)
			cfg.RetryBackoff = 0
		})
		h.RecordSpan(sp)
		h.HarvestNow(context.Background())
		if (test.shouldRetry && 2 != posts) || (!test.shouldRetry && 1 != posts) {
			t.Error("incorrect number of posts", posts)
		}
	}
}

func Test429RetryAfterUsesConfig(t *testing.T) {
	// Test when resp code is 429, retry backoff uses value from config if:
	// * Retry-After header not set
	// * Retry-After header not parsable
	// * Retry-After header delay is less than config retry backoff
	var posts int
	var start time.Time
	tm := time.Date(2014, time.November, 28, 1, 1, 0, 0, time.UTC)
	span := Span{TraceID: "id", ID: "id", Name: "span1", Timestamp: tm}

	roundTripper := func(retryHeader string) roundTripperFunc {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			posts++
			if posts > 1 {
				if since := time.Since(start); since > time.Second {
					t.Errorf("incorrect retry backoff used, since=%v", since)
				}
				return emptyResponse(200), nil
			}
			start = time.Now()
			resp := emptyResponse(429)
			resp.Header = http.Header{}
			resp.Header.Add("Retry-After", retryHeader)
			return resp, nil
		})
	}

	h := NewHarvester(func(cfg *Config) {
		cfg.Client.Transport = roundTripper("")
		cfg.APIKey = "key"
		cfg.RetryBackoff = 0
	})
	h.RecordSpan(span)
	h.HarvestNow(context.Background())
	if posts != 2 {
		t.Error("incorrect number of posts", posts)
	}

	posts = 0
	h = NewHarvester(func(cfg *Config) {
		cfg.Client.Transport = roundTripper("hello world!")
		cfg.APIKey = "key"
		cfg.RetryBackoff = 0
	})
	h.RecordSpan(span)
	h.HarvestNow(context.Background())
	if posts != 2 {
		t.Error("incorrect number of posts", posts)
	}

	posts = 0
	h = NewHarvester(func(cfg *Config) {
		cfg.Client.Transport = roundTripper("0")
		cfg.APIKey = "key"
		cfg.RetryBackoff = 2
	})
	h.RecordSpan(span)
	h.HarvestNow(context.Background())
	if posts != 2 {
		t.Error("incorrect number of posts", posts)
	}
}

func TestResponseNeedsRetry(t *testing.T) {
	testcases := []struct {
		headerRetry   string
		respCode      int
		expectRetry   bool
		expectBackoff time.Duration
	}{
		{
			headerRetry:   "2",
			respCode:      202,
			expectRetry:   false,
			expectBackoff: 0,
		},
		{
			headerRetry:   "2",
			respCode:      200,
			expectRetry:   false,
			expectBackoff: 0,
		},
		{
			headerRetry:   "2",
			respCode:      413,
			expectRetry:   false,
			expectBackoff: 0,
		},
		{
			headerRetry:   "",
			respCode:      429,
			expectRetry:   true,
			expectBackoff: time.Second,
		},
		{
			headerRetry:   "hello",
			respCode:      429,
			expectRetry:   true,
			expectBackoff: time.Second,
		},
		{
			headerRetry:   "0.5",
			respCode:      429,
			expectRetry:   true,
			expectBackoff: time.Second,
		},
		{
			headerRetry:   "2",
			respCode:      429,
			expectRetry:   true,
			expectBackoff: 2 * time.Second,
		},
	}

	h := NewHarvester(configTesting, func(cfg *Config) {
		cfg.RetryBackoff = time.Second
	})
	for _, test := range testcases {
		resp := response{
			statusCode: test.respCode,
			retryAfter: test.headerRetry,
		}
		actualRetry, actualBackoff := resp.needsRetry(&h.config)
		if actualRetry != test.expectRetry {
			t.Errorf("incorrect retry value found, actualRetry=%t, expectRetry=%t", actualRetry, test.expectRetry)
		}
		if actualBackoff != test.expectBackoff {
			t.Errorf("incorrect retry value found, actualBackoff=%v, expectBackoff=%v", actualBackoff, test.expectBackoff)
		}
	}
}

func TestNoDataNoHarvest(t *testing.T) {
	roundTripper := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		t.Error("harvest should not have been run")
		return emptyResponse(200), nil
	})

	h := NewHarvester(func(cfg *Config) {
		cfg.HarvestPeriod = 0
		cfg.Client.Transport = roundTripper
		cfg.APIKey = "APIKey"
		cfg.RetryBackoff = 0
	})
	h.HarvestNow(context.Background())
}

func TestNewRequestErrorNoPost(t *testing.T) {
	// Test that when newRequest returns an error, no post is made
	roundTripper := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		t.Error("no post should not have been run")
		return emptyResponse(200), nil
	})

	h := NewHarvester(func(cfg *Config) {
		cfg.HarvestPeriod = 0
		cfg.Client.Transport = roundTripper
		cfg.APIKey = "APIKey"
		cfg.RetryBackoff = 0
		cfg.MetricsURLOverride = "t h i s  i s  n o t  a  h o s t%"
	})
	h.RecordMetric(Count{})
	h.HarvestNow(context.Background())
}

func TestRecordMetricNil(t *testing.T) {
	var h *Harvester
	h.RecordMetric(Count{})
}

func TestRecordMetricDisabled(t *testing.T) {
	h := NewHarvester(func(cfg *Config) {
		cfg.APIKey = ""
		cfg.HarvestPeriod = 0
	})
	h.RecordMetric(Count{})
	if 0 != len(h.rawMetrics) {
		t.Error(h.rawMetrics)
	}
}

func TestBeforeHarvestFunc(t *testing.T) {
	var calls int
	h := NewHarvester(configTesting, func(cfg *Config) {
		cfg.BeforeHarvestFunc = func(h *Harvester) {
			calls++
		}
	})
	h.HarvestNow(context.Background())
	if 1 != calls {
		t.Error("BeforeHarvestFunc not called")
	}
}

func TestRecordSpanZeroTimestamp(t *testing.T) {
	h := NewHarvester(func(cfg *Config) {
		cfg.HarvestPeriod = 0
		cfg.APIKey = "APIKey"
	})
	if err := h.RecordSpan(Span{
		ID:      "id",
		TraceID: "traceid",
	}); err != nil {
		t.Fatal(err)
	}
	if s := h.spans[0]; s.Timestamp.IsZero() {
		t.Fatal(s.Timestamp)
	}
}

func TestHarvestAuditLog(t *testing.T) {
	roundTripper := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return emptyResponse(200), nil
	})

	var audit map[string]interface{}

	h := NewHarvester(func(cfg *Config) {
		cfg.HarvestPeriod = 0
		cfg.APIKey = "APIKey"
		cfg.Client.Transport = roundTripper
		cfg.AuditLogger = func(fields map[string]interface{}) {
			audit = fields
		}
	})
	h.RecordMetric(Count{})
	h.HarvestNow(context.Background())
	if u := audit["url"]; u != "https://metric-api.newrelic.com/metric/v1" {
		t.Fatal(u)
	}
	// We can't test "data" against a fixed string because of the dynamic
	// timestamp.
	if d := audit["data"]; !strings.Contains(string(d.(jsonString)), `"metrics":[{"name":"","type":"count","value":0}]`) {
		t.Fatal(d)
	}
}
