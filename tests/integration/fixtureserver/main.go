// Command fixtureserver runs the fixture search site and proxies from
// tests/integration/fixture as standalone listeners, for the Docker
// end-to-end run (deploy/docker-compose.test.yml). Each port is one fixture
// proxy; the fictional site (fixture.Host) is only reachable through them.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/HenryMorganDibie/serp-harvester/tests/integration/fixture"
)

func main() {
	good := flag.String("good", ":8081", "listen address of a well-behaved proxy")
	captcha := flag.String("captcha", ":8082", "listen address of a proxy that always gets a CAPTCHA page")
	limited := flag.String("ratelimit-once", ":8083", "listen address of a proxy rate-limited on its first request")
	flag.Parse()

	serve := func(addr string, b fixture.Behavior) {
		log.Printf("fixture proxy %q on %s", b, addr)
		log.Fatal(http.ListenAndServe(addr, fixture.NewProxy(b)))
	}
	go serve(*captcha, fixture.Captcha)
	go serve(*limited, fixture.RateLimitOnce)
	serve(*good, fixture.Good)
}
