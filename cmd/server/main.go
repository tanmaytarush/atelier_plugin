package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/tanmaydikshit/vto-shopify-app/internal/config"
	"github.com/tanmaydikshit/vto-shopify-app/internal/shopifyauth"
	"github.com/tanmaydikshit/vto-shopify-app/internal/store"
	"github.com/tanmaydikshit/vto-shopify-app/internal/vto"
)

func main() {
	// PORT is read inline for now. Turn 1.1 (internal / config) replaces this
	// with validated configuration loading so secrets fall fast at startup.
	// port := os.Getenv("PORT")
	// if port == "" {
	// 	port = "8080"
	// }

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.NewSQLiteStore(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	oauth := shopifyauth.NewOAuthHandler(cfg, st)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/auth/install", oauth.Install)
	r.Get("/auth/callback", oauth.Callback)

	// Liveness probe. Hosts (Fly.io, Render) and shopify tunnels hit this to
	// confirm the process is running. Keep it dependency free - it must answer
	// even if DB or N8N is down.

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	r.Route("/apps/vto", func(pr chi.Router) {
		pr.Use(shopifyauth.VerifyAppProxy(cfg.ShopifyAPISecret))
		pr.Post("/upload", vtoHandler.Upload) // TODO: add upload handler
		pr.Post("/tryon", vtoHandler.TryOn)   // TODO: add try-on handler
	})

	n8n := vto.NewClient(cfg.VTOUploadWebhook, cfg.VTOTryonWebhook)
	// n8n gets passed into the upload/tryon handlers in 1.6/1.7.

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second, // revisit once try-on render latency
		IdleTimeout:  60 * time.Second,
	}

	// Run the server in its own goroutine so main can block on shutdown signals
	go func() {
		log.Printf("Listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Block until an interrupt or terminate signal arrives.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down...")

	// Give in-flight requests upto 10s to finish before forcing exit.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	defer cancel()
	// if error exists, log and force the server to stop
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Graceful shutdown failed: %v", err)
	}

	log.Println("Server stopped")
}
