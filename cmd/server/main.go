package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"pianoindex/internal/bitmap"
	"pianoindex/internal/dataset"
	"pianoindex/internal/idx"
	"pianoindex/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	n := flag.Int("n", 300000, "number of pianos")
	seed := flag.Int64("seed", 42, "dataset seed")
	mappedPath := flag.String("mapped", "", "path of an mmap-serialized bitmap to demonstrate in-place queries")
	flag.Parse()

	start := time.Now()
	ps, _ := dataset.Generate(*seed, *n)
	ix := idx.Build(ps)
	log.Printf("indexed %d pianos in %v, %d bitmaps, %.1f MiB",
		len(ps), time.Since(start), ix.Memory().BitmapsCount, float64(ix.Memory().TotalByte)/(1<<20))

	if *mappedPath != "" {
		if err := os.MkdirAll(filepath.Dir(*mappedPath), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := ix.All.Serialize(*mappedPath, uint64(len(ps))); err != nil {
			log.Fatalf("serialize: %v", err)
		}
		t0 := time.Now()
		m, err := bitmap.OpenMapped(*mappedPath)
		if err != nil {
			log.Fatalf("mmap open: %v", err)
		}
		defer m.Close()
		log.Printf("mmap bitmap %s: %d rows readable in %v without deserialization",
			*mappedPath, m.Bitmap().Cardinality(), time.Since(t0))
	}

	srv := web.New(ix)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("listening on http://%s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Println("shutting down")
}
