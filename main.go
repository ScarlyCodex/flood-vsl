package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

var (
	targetURL       string
	totalRequests   int
	maxConcurrency  int
	timeoutMs       int
	outputFile      string
	maxErrorRate    float64 = 0.2
	maxErrorCount   int     = 500
	sentCount       int32
	errorCount      int32
	abortSignal     int32
	status200Count  int32
	status429Count  int32
	status500Count  int32
	responseTimes   []time.Duration
	rtMutex         sync.Mutex
)

func main() {
	flag.StringVar(&targetURL, "u", "", "Target URL (e.g. https://example.com/api)")
	flag.IntVar(&totalRequests, "n", 5000, "Total number of requests")
	flag.IntVar(&maxConcurrency, "c", 100, "Maximum concurrency")
	flag.IntVar(&timeoutMs, "t", 0, "Timeout in ms")
	flag.StringVar(&outputFile, "o", "", "Output log file (optional)")
	flag.Parse()

	if targetURL == "" {
		fmt.Println(colorRed + "Error:" + colorReset + " -u is required")
		os.Exit(1)
	}

	var out io.Writer = os.Stdout
	if outputFile != "" {
		f, err := os.Create(outputFile)
		if err != nil {
			fmt.Printf(colorRed+"Error creating %s: %v"+colorReset+"\n", outputFile, err)
			os.Exit(1)
		}
		defer f.Close()
		out = io.MultiWriter(os.Stdout, f)
	}

	client := &http.Client{}
	if timeoutMs > 0 {
		client.Timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	fmt.Fprintf(out, "%sStarting flood test on: %s%s\n", colorCyan, targetURL, colorReset)

	done := make(chan bool)
	go showProgress(done, out)

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < totalRequests && atomic.LoadInt32(&abortSignal) == 0; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			t0 := time.Now()
			resp, err := client.Get(targetURL)
			lat := time.Since(t0)

			atomic.AddInt32(&sentCount, 1)
			rtMutex.Lock()
			responseTimes = append(responseTimes, lat)
			rtMutex.Unlock()

			if err != nil {
				atomic.AddInt32(&errorCount, 1)
			} else {
				defer resp.Body.Close()
				switch resp.StatusCode {
				case 200:
					atomic.AddInt32(&status200Count, 1)
				case 429:
					atomic.AddInt32(&status429Count, 1)
					atomic.AddInt32(&errorCount, 1)
				default:
					if resp.StatusCode >= 500 {
						atomic.AddInt32(&status500Count, 1)
						atomic.AddInt32(&errorCount, 1)
					}
				}
			}

			if float64(atomic.LoadInt32(&errorCount))/float64(atomic.LoadInt32(&sentCount)) > maxErrorRate ||
				atomic.LoadInt32(&errorCount) >= int32(maxErrorCount) {
				atomic.StoreInt32(&abortSignal, 1)
			}
		}()
	}

	wg.Wait()
	done <- true
	totalDur := time.Since(start)

	fmt.Fprintln(out, "\n"+colorCyan+"📋 Test Summary"+colorReset)
	fmt.Fprintln(out, "--------------------------------")
	fmt.Fprintf(out, "%sTotal sent:%s      %d\n", colorGreen, colorReset, sentCount)
	fmt.Fprintf(out, "%s200 OK:%s         %d\n", colorGreen, colorReset, status200Count)
	fmt.Fprintf(out, "%s429 Too Many:%s   %d\n", colorYellow, colorReset, status429Count)
	fmt.Fprintf(out, "%s5xx Errors:%s     %d\n", colorRed, colorReset, status500Count)
	fmt.Fprintf(out, "Total duration: %.2fs\n", totalDur.Seconds())

	analyzeDegradation(out)

	if atomic.LoadInt32(&abortSignal) == 1 {
		fmt.Fprintln(out, colorYellow+"⚠️  Flood aborted: high error rate"+colorReset)
	} else {
		fmt.Fprintln(out, colorGreen+"✅ Flood completed. Target responsive"+colorReset)
	}
}

func showProgress(done <-chan bool, out io.Writer) {
	for {
		select {
		case <-done:
			fmt.Fprintf(out, "\r✓ Completed: %d requests.\n", sentCount)
			return
		default:
			c := atomic.LoadInt32(&sentCount)
			e := atomic.LoadInt32(&errorCount)
			p := float64(c) / float64(totalRequests) * 100
			s2 := atomic.LoadInt32(&status200Count)
			s4 := atomic.LoadInt32(&status429Count)
			s5 := atomic.LoadInt32(&status500Count)
			fmt.Fprintf(out, "\rProgress: %d/%d (%.1f%%) - 200:%d 429:%d 5xx:%d err:%d",
				c, totalRequests, p, s2, s4, s5, e)
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func analyzeDegradation(out io.Writer) {
	rtMutex.Lock()
	defer rtMutex.Unlock()
	n := len(responseTimes)
	if n < 2 {
		fmt.Fprintln(out, "Not enough data for degradation analysis")
		return
	}

	bucket := n / 10
	if bucket < 1 {
		bucket = 1
	}

	var sumFirst, sumLast int64
	for i := 0; i < bucket && i < n; i++ {
		sumFirst += int64(responseTimes[i])
	}
	for i := n - bucket; i < n; i++ {
		sumLast += int64(responseTimes[i])
	}

	avgFirst := time.Duration(sumFirst / int64(bucket))
	avgLast := time.Duration(sumLast / int64(bucket))

	fmt.Fprintf(out,
		"Avg lat_first: %s%s%s  Avg lat_last: %s%s%s\n",
		colorCyan, avgFirst, colorReset,
		colorCyan, avgLast, colorReset,
	)
	if avgLast > avgFirst*2 {
		fmt.Fprintln(out, colorRed+"⚠️  Significant degradation detected"+colorReset)
	} else {
		fmt.Fprintln(out, colorGreen+"✅ No significant degradation"+colorReset)
	}
}