package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"checkout-api/handlers"
	"checkout-api/middleware"
	"checkout-api/store"

	"github.com/golang-jwt/jwt/v5" // <- for JWT parsing
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func AuthMiddleWare(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationHeaderStr := r.Header.Get("Authorization")
		if authorizationHeaderStr == "" {
			fmt.Println("DEBUG: Authorization header is missing")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		scheme := "Bearer "
		if len(authorizationHeaderStr) < len(scheme) {
			fmt.Println("DEBUG: Authorization header too short")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		userScheme := authorizationHeaderStr[:len(scheme)]
		if !strings.EqualFold(scheme, userScheme) {
			fmt.Printf("DEBUG: Wrong scheme. Expected 'bearer ', got %q\n", userScheme)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		userJWT := authorizationHeaderStr[len(scheme):]
		var claims jwt.RegisteredClaims
		_, err := jwt.ParseWithClaims(
			userJWT,
			&claims,
			func(t *jwt.Token) (any, error) {
				return []byte(handlers.SigningSecret), nil
			},
			jwt.WithValidMethods([]string{"HS256"}),
		)
		if err != nil {
			// This will tell you if the token is EXPIRED or the SIGNATURE is wrong
			fmt.Printf("DEBUG: JWT parse error: %v\n", err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		userID, err := strconv.Atoi(claims.Subject)
		if err != nil {
			fmt.Printf("DEBUG: Failed to parse userID from subject: %v\n", err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), handlers.UserContextKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func main() {

	godotenv.Load()

	// 1. Get the connection string from the environment
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set in .env file")
	}

	// 2. Hash your password (existing logic)
	password := []byte("testing12345")
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Generated Hash: %x\n", hash)

	ctx := context.Background()

	// 3. FIX: Use the 'dsn' variable instead of the hardcoded localhost string
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("Unable to create connection pool: %v", err)
	}
	defer pool.Close()

	err = pool.Ping(ctx)
	if err != nil {
		log.Fatalf("Unable to ping database: %v", err)
	}
	fmt.Println("Successfully connected to Neon database!")

	// Rest of your handlers...
	postgresStore := store.NewPostgresStore(pool)
	h := handlers.NewHandler(postgresStore)

	// cart
	http.Handle("/user/cart", AuthMiddleWare(http.HandlerFunc(h.GetUserCart)))
	http.Handle("/user/cart/items/{item_id}", AuthMiddleWare(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			h.UpsertCartItem(w, r)
		case http.MethodDelete:
			h.RemoveCartItem(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	// orders
	http.Handle("/orders", AuthMiddleWare(http.HandlerFunc(h.CreateOrder)))

	// items
	http.HandleFunc("/items", h.GetItems)
	http.HandleFunc("/items/", h.GetItemByID)

	// users
	http.HandleFunc("POST /signup", h.CreateUser)
	http.HandleFunc("POST /login", h.LoginUser)

	// TODO: implement Get RefreshToken
	http.Handle("GET /token", AuthMiddleWare(http.HandlerFunc(h.IssueJWT)))
	http.HandleFunc("POST /token", h.IssueJWT)

	fmt.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", middleware.CORSMiddleware(middleware.DelayMiddleware(http.DefaultServeMux))))
}
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Allow localhost AND your Vercel URL
		if origin == "http://localhost:5173" || strings.HasSuffix(origin, "https://xsolla-alanis-storefront-m4zp.vercel.app/") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
