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

	// n8n client + VTO HTTP handler. Declared before the route group below
	// because the route registration references vtoHandler.
	n8n := vto.NewClient(cfg.VTOUploadWebhook, cfg.VTOTryonWebhook)
	vtoHandler := vto.NewHandler(n8n)

	r.Route("/apps/vto", func(pr chi.Router) {
		pr.Use(shopifyauth.VerifyAppProxy(cfg.ShopifyAPISecret))
		pr.Post("/upload", vtoHandler.Upload)
		pr.Post("/tryon", vtoHandler.TryOn)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,  // room for a ~10MB photo upload on mobile
		WriteTimeout: 130 * time.Second, // must exceed the n8n client's 120s synchronous try-on timeout
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
