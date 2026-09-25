package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/HenryMorganDibie/web-harvester/internal/extract"
	"github.com/HenryMorganDibie/web-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/web-harvester/internal/metrics"
	"github.com/HenryMorganDibie/web-harvester/internal/proxy"
	"github.com/HenryMorganDibie/web-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/web-harvester/internal/robots"
	"github.com/HenryMorganDibie/web-harvester/internal/worker"
)

// A client-side app: the server sends an empty shell and the items only
// exist once the script runs.
const appShell = `<html><head><title>App</title></head><body><div id="root"></div>
<script>
  var root = document.getElementById("root");
  [["Kettle", "29.99"], ["Toaster", "19.99"]].forEach(function (p) {
    var d = document.createElement("div");
    d.className = "item";
    d.innerHTML = "<h3>" + p[0] + "</h3><span class=price>" + p[1] + "</span>";
    root.appendChild(d);
  });
  var a = document.createElement("a"); a.href = "/static"; a.textContent = "static"; root.appendChild(a);
</script></body></html>`

func TestCrawl_AutoRendersOnlyAppShellsInChromium(t *testing.T) {
	if os.Getenv("SERP_HARVESTER_PLAYWRIGHT") != "1" {
		t.Skip("set SERP_HARVESTER_PLAYWRIGHT=1 (with Playwright's Chromium installed) to run browser tests")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, appShell)
	})
	mux.HandleFunc("/static", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Static</title></head><body><p>Server-rendered page with plenty of text in it, `+
			`enough that it is clearly not an empty application shell waiting for a script to fill it in.</p>`+
			`<div class="item"><h3>Kettle</h3><span class="price">29.99</span></div></body></html>`)
	})
	mux.HandleFunc("/challenge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<html><head><title>Just a moment...</title></head><body><form id="challenge-form"></form></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	counters := &metrics.Counters{Browser: &metrics.BrowserCounters{}}
	browser, err := fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
		Timeout: 15 * time.Second, PoolSize: 2, Headless: true, Generic: true,
		ExecutablePath: os.Getenv("SERP_HARVESTER_CHROMIUM_PATH"), Metrics: counters.Browser,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	shellAware, _ := fetcher.NewHTTPFetcher("", 5*time.Second)
	shellAware.Generic, shellAware.DetectAppShells = true, true
	plain, _ := fetcher.NewHTTPFetcher("", 5*time.Second)
	plain.Generic = true

	pool := &worker.Pool{
		Concurrency: 2, MaxRetries: 0,
		Fetcher: &fetcher.FailoverFetcher{Primary: shellAware, Secondary: browser,
			OnFailover: func(fetcher.Outcome) { counters.IncFailover() }},
		ProxyPool: proxy.NewPool(nil, 1000, time.Hour), Limiter: ratelimit.New(1000, 1),
		Metrics: counters, UserAgent: "test-crawler/1.0",
	}
	robotsPool := *pool
	robotsPool.Fetcher = plain
	sink := &memSink{}
	c := &Crawler{Pool: pool, Robots: &robots.Cache{Agent: "test-crawler", Get: RobotsGetter(&robotsPool)}, Sink: sink}

	spec := extract.Spec{Items: &extract.ItemsSpec{Selector: ".item", Fields: map[string]extract.Rule{
		"name": {Selector: "h3"}, "price": {Selector: ".price"},
	}}}
	stats, err := c.Run(context.Background(), Target{Name: "app", StartURLs: []string{srv.URL + "/app"}, MaxDepth: 1, Render: "auto", Extract: spec})
	if err != nil {
		t.Fatal(err)
	}
	pages := sink.byURL()
	app := pages[srv.URL+"/app"]
	if app == nil || len(app.Items) != 2 || app.Items[0]["name"] != "Kettle" {
		t.Fatalf("app page = %+v; its items only exist after rendering", app)
	}
	if pages[srv.URL+"/static"] == nil {
		t.Error("the link the script added was not followed")
	}
	if counters.Failovers != 1 || counters.Browser.Snapshot().Launches != 1 {
		t.Errorf("failovers = %d; only the app shell should be rendered, the static page fetched over HTTP", counters.Failovers)
	}
	if stats.Fetched != 2 {
		t.Errorf("stats = %+v", stats)
	}

	// A challenge is recorded as such in the browser too, never solved.
	pool.Fetcher = browser
	pool.ProxyPool = proxy.NewPool(nil, 1000, time.Hour)
	stats, _ = c.Run(context.Background(), Target{Name: "blocked", StartURLs: []string{srv.URL + "/challenge"}, Render: "browser"})
	if stats.Failed != 1 || counters.Browser.Snapshot().BlockedChallenge != 1 {
		t.Errorf("stats = %+v, browser blocked_challenge = %d", stats, counters.Browser.Snapshot().BlockedChallenge)
	}
}
