package main

import (
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"github.com/mitchellh/mapstructure"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const charsetRand = "abcdefghijklmnopqrstuvwxyz0123456789"

var seededRand *rand.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))

const (
	namespace = "emarsys" // For Prometheus metrics.
)

var (
	listeningAddress = flag.String("telemetry.address", ":9485", "Address on which to expose metrics.")
	metricsEndpoint  = flag.String("telemetry.endpoint", "/metrics", "Path under which to expose metrics.")
	insecure         = flag.Bool("insecure", true, "Ignore server certificate if using https")
)

var (
	queueLabelNames = []string{"queue_name"} // Label name(s) for queue
	emarsysuri      string
	emarsysuser     string
	emarsyspass     string
	scrapeError     float64
)

// Type for Emarsys metrics
type emarsysMetrics struct {
	Mail_Generate, Pmta_Send, Automation_Center, Ac_Lite, Push_Wrapper, Pushwoosh float64
}

type queuesSizes struct {
	name string
	size float64
}

type queueCollector struct {
	queues *prometheus.Desc
}

// Exporter collects emarsys stats from the given URI and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI    string
	mutex  sync.RWMutex
	client *http.Client

	exporterScrapeError *prometheus.Desc
	scrapeFailures      prometheus.Counter

	mail_generate     *prometheus.Desc
	pmta_send         *prometheus.Desc
	automation_center *prometheus.Desc
	ac_lite           *prometheus.Desc
	push_wrapper      *prometheus.Desc
	pushwoosh         *prometheus.Desc
	emarsysUp         prometheus.Gauge
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string) *Exporter {
	return &Exporter{
		URI: uri,
		exporterScrapeError: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "exporter", "last_scrape_error"),
			"Whether the last scrape of metrics resulted in an error (1 for error, 0 for success).",
			nil, nil,
		),
		scrapeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrape_failures_total",
			Help:      "Number of errors while scraping emarsys.",
		}),
		mail_generate: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "mail_generate"),
			"Emarsys Mail Generate.",
			nil, nil,
		),
		pmta_send: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "pmta_send"),
			"Emarsys PMTA Send.",
			nil, nil,
		),
		automation_center: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "automation_center"),
			"Emarsys Automation Center.",
			nil, nil,
		),
		ac_lite: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "ac_lite"),
			"Emarsys AC Lite.",
			nil, nil,
		),
		push_wrapper: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "push_wrapper"),
			"Emarsys Push Wrapper.",
			nil, nil,
		),
		pushwoosh: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "pushwoosh"),
			"Emarsys PushWoosh.",
			nil, nil,
		),
		emarsysUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Whether the emarsys is up.",
		}),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
			},
		},
	}
}

// Describe describes all the metrics ever exported by the emarsys exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.exporterScrapeError
	e.scrapeFailures.Describe(ch)

	e.emarsysUp.Describe(ch)

	ch <- e.mail_generate
	ch <- e.pmta_send
	ch <- e.automation_center
	ch <- e.ac_lite
	ch <- e.push_wrapper
	ch <- e.pushwoosh
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
	resp, err := e.client.Get(e.URI)
	if err != nil {
		e.emarsysUp.Set(0)
		ch <- prometheus.MustNewConstMetric(e.exporterScrapeError, prometheus.GaugeValue, 1)
		return err
	}
	e.emarsysUp.Set(1)
	defer resp.Body.Close()

	emarsys, err := GetEmarsysMetrics()
	if err != nil {
		return err
	}

	for _, s := range emarsys {
		ch <- prometheus.MustNewConstMetric(e.mail_generate, prometheus.GaugeValue, s.Mail_Generate)
		ch <- prometheus.MustNewConstMetric(e.pmta_send, prometheus.GaugeValue, s.Pmta_Send)
		ch <- prometheus.MustNewConstMetric(e.automation_center, prometheus.GaugeValue, s.Automation_Center)
		ch <- prometheus.MustNewConstMetric(e.ac_lite, prometheus.GaugeValue, s.Ac_Lite)
		ch <- prometheus.MustNewConstMetric(e.push_wrapper, prometheus.GaugeValue, s.Push_Wrapper)
		ch <- prometheus.MustNewConstMetric(e.pushwoosh, prometheus.GaugeValue, s.Pushwoosh)
	}

	if scrapeError != 0 {
		ch <- prometheus.MustNewConstMetric(e.exporterScrapeError, prometheus.GaugeValue, scrapeError)
	}

	lastScrapeError := scrapeError

	if lastScrapeError == 0 {
		ch <- prometheus.MustNewConstMetric(e.exporterScrapeError, prometheus.GaugeValue, scrapeError)
	}

	return nil
}

// Collect fetches the stats from configured emarsys location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()
	if err := e.collect(ch); err != nil {
		log.Errorf("Error scraping emarsys: %s", err)
		e.scrapeFailures.Inc()
		e.scrapeFailures.Collect(ch)
	}
	e.emarsysUp.Collect(ch)
	return
}

// load content from URI (JSON)
func getContent(uri string) (body string, err error) {
	scrapeError = 0

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: *insecure,
			},
		},
	}

	req, err := http.NewRequest(http.MethodGet, uri, nil)

	if err != nil {
		scrapeError = 1
		log.Error(err)
		return "", err
	}

	req.Header.Add("X-WSSE", getWSSE())
	req.Header.Add("Content-Type", "application/json")

	res, err := httpClient.Do(req)

	if err != nil {
		scrapeError = 1
		log.Error(err)
		return "", err
	}

	if res.StatusCode < 200 || res.StatusCode >= 400 {
		log.Error(res.Status)
		scrapeError = 1
		return "", errors.New("HTTP return code: " + strconv.Itoa(res.StatusCode))
	}

	content, err := ioutil.ReadAll(res.Body)

	if err != nil {
		scrapeError = 1
		log.Error(err)
		return "", errors.New("Error while getting HTTP content / reading body.")
	}

	defer res.Body.Close()

	return string(content), err
}

// extracting emarsys metrics from JSON to struct
func GetEmarsysMetrics() ([]emarsysMetrics, error) {
	body, e := getContent(emarsysuri)
	if e != nil {
		scrapeError = 1
		return nil, e
	}

	var f interface{}
	err := json.Unmarshal([]byte(body), &f)
	if err != nil {
		log.Error(err)
		scrapeError = 1
		return nil, nil
	}

	m := f.(map[string]interface{})

	metrics := []emarsysMetrics{}

	for name, value := range m {
		if name == "latency" {
			res, err := EmarsysStructFromMap(value.(map[string]interface{}))
			if err != nil {
				scrapeError = 1
				log.Error(err)
			}
			metrics = append(metrics, res)
		}
	}

	if len(metrics) == 0 {
		scrapeError = 1
		log.Error("There are not Emarsys Metrics to export.")
	}

	return metrics, nil
}

// converting map interface to struct
func EmarsysStructFromMap(m map[string]interface{}) (emarsysMetrics, error) {
	var result emarsysMetrics
	err := mapstructure.Decode(m, &result)
	return result, err
}

// calculating a random string using characters from the constant charsetRand
func randomize(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = charsetRand[seededRand.Intn(len(charsetRand))]
	}
	return string(b)
}

// creating a WSSE token
func getWSSE() string {
	nonce := randomize(64, charsetRand)
	now := time.Now().UTC().Format(time.RFC3339)
	digestString := nonce + now + emarsyspass
	digestHash := sha1.New()
	digestHash.Write([]byte(digestString))
	digestBS := digestHash.Sum(nil)
	digest := base64.URLEncoding.EncodeToString([]byte(hex.EncodeToString(digestBS)))
	wsse := "UsernameToken Username=\"" + emarsysuser + "\", PasswordDigest=\"" + digest + "\", Nonce=\"" + nonce + "\", Created=\"" + now + "\""
	return wsse
}

func main() {
	// URI command line parameters for this Exporter set in main function and not globaly because we want to use the current hostname here. This can't be done globaly.
	emarsysURI := flag.String("emarsys-uri", "https://api.emarsys.net/api/latency-monitor/metrics", "URI to Emarsys JSON.")

	flag.Parse()

	emarsysuri = *emarsysURI
	emarsysuser = os.Getenv("EMARSYSUSER")
	emarsyspass = os.Getenv("EMARSYSPASS")

	if emarsysuser == "" || emarsyspass == "" {
		log.Fatal("Please set Emarsys username and password!")
	}

	exporter := NewExporter(*emarsysURI)
	prometheus.MustRegister(exporter)

	log.Infof("Starting Server...")
	log.Infof("Listening on %s", *listeningAddress)
	log.Infof("Emarsys URI: %s", emarsysuri)
	http.Handle(*metricsEndpoint, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Emarsys Exporter</title></head>
			<body>
			<h1>Emarsys Exporter</h1>
			<p><a href="` + *metricsEndpoint + `">Metrics</a></p>
			</body>
			</html>`))
	})

	log.Fatal(http.ListenAndServe(*listeningAddress, nil))
}
