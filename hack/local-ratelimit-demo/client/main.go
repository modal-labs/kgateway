// Toy load client for the local rate limit demo.
//
// Sends N requests to a URL (optionally with a header) and prints a tally of
// status codes so you can see when the gateway starts returning 429s.
//
//	go run ./hack/local-ratelimit-demo/client -url http://127.0.0.1:8080/api/foo -n 20 -header x-workspace-id=ws-a
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/", "URL to hit")
	n := flag.Int("n", 20, "number of requests")
	interval := flag.Duration("interval", 0, "pause between requests (0 = as fast as possible)")
	header := flag.String("header", "", "extra request header as key=value (repeatable with commas)")
	verbose := flag.Bool("v", false, "print each response")
	flag.Parse()

	req, err := http.NewRequest(http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, kv := range strings.Split(*header, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			req.Header.Set(k, v)
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}
	counts := map[int]int{}
	var order []int
	start := time.Now()
	for i := 0; i < *n; i++ {
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "req %d: %v\n", i+1, err)
			counts[0]++
			order = append(order, 0)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		counts[resp.StatusCode]++
		order = append(order, resp.StatusCode)
		if *verbose {
			fmt.Printf("req %2d: %d  x-ratelimit-limit=%s remaining=%s\n", i+1, resp.StatusCode,
				resp.Header.Get("x-ratelimit-limit"), resp.Header.Get("x-ratelimit-remaining"))
		}
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}

	codes := make([]int, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	fmt.Printf("%s %s (%d requests in %s):", req.Header.Get(strings.Split(*header, "=")[0]), *url, *n, time.Since(start).Round(time.Millisecond))
	for _, c := range codes {
		fmt.Printf("  %d=%d", c, counts[c])
	}
	fmt.Println()
	seq := make([]string, len(order))
	for i, c := range order {
		seq[i] = fmt.Sprint(c)
	}
	fmt.Println("  sequence:", strings.Join(seq, " "))
}
