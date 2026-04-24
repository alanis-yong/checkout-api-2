package handlers

import (
	"checkout-api/models"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
)

// TODO Bonus: this looks dangerous maybe you can save it in a .env file
// then add it to .gitignore so that your secrets are not pushed to the server
// try https://github.com/spf13/viper
type MyCustomClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

var SigningSecret string

func init() {
	// Load .env file first
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on system environment variables")
	}

	SigningSecret = os.Getenv("SIGNING_SECRET")
	if SigningSecret == "" {
		log.Fatal("SIGNING_SECRET not set! JWT cannot be issued.")
	}
	log.Println("SigningSecret loaded successfully") // debug
}

type ContextKey string

const UserContextKey = ContextKey("userID")

func generateRefreshToken() (string, error) {
	b := make([]byte, 32) // 32 bytes = 256 bits
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ItemStore defines the data operations the handler needs.
type ItemStore interface {
	GetItems(ctx context.Context) ([]*models.Item, error)
	GetItem(ctx context.Context, id int) (*models.Item, error)
	CreateOrder(ctx context.Context, userID int, items []models.LineItem, total int, status string) (*models.Order, error)
	UpdateOrderStatus(ctx context.Context, orderID int, status string) error
	UpsertCartItem(ctx context.Context, userID int, itemID int, quantity int) error
	GetUserCart(ctx context.Context, userID int) ([]models.CartItemResponse, error)
	DeleteUserCart(ctx context.Context, userID int) error
	RemoveCartItem(ctx context.Context, userID int, itemID int) error
	SaveUser(ctx context.Context, email string, hash []byte) error
	FindUserByEmail(ctx context.Context, email string) (models.User, error)
	ValidateRefreshToken(ctx context.Context, token string) (int, error)
	SaveRefreshToken(ctx context.Context, userID int, token string) error
	DeactivateRefreshToken(ctx context.Context, token string) error
	FindIdempotencyKey(ctx context.Context, id string) (*models.IdempotencyRecord, error)
	SaveIdempotencyKey(ctx context.Context, id string, status_code int, expiry time.Time, response []byte) error
	DeleteIdempotencyKey(ctx context.Context, id string) error
	FindUserByID(ctx context.Context, id int) (models.User, error)
}

// Handler holds dependencies for HTTP handlers.
type Handler struct {
	store ItemStore
}

// NewHandler creates a Handler with the given store.
func NewHandler(s ItemStore) *Handler {
	return &Handler{
		store: s,
	}
}

// mockProcessPayment simulates a payment provider call.
func mockProcessPayment(amount int) PaymentResult {
	if amount > 0 && amount < 1000000 {
		return PaymentResult{
			Success:       true,
			TransactionID: fmt.Sprintf("txn_%d", time.Now().UnixNano()),
		}
	}
	return PaymentResult{
		Success: false,
		Error:   "Payment declined",
	}
}

func (h *Handler) UpsertCartItem(w http.ResponseWriter, r *http.Request) {
	// TODO: this looks like it can be extracted as a commonly used
	// Explore the middleware pattern in net/http and see if you can extract authentication logic
	// into it's own handler(middleware)

	userID := r.Context().Value(UserContextKey).(int)

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 { // /user/cart/items/1 → parts[3] = "1"
		http.Error(w, "missing item_id", http.StatusBadRequest)
		return
	}
	itemIDStr := parts[len(parts)-1]

	itemID, err := strconv.Atoi(itemIDStr)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
			Message: "item_id must be integer",
		})
		return
	}

	var req UpsertCartItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Quantity < 0 {
		http.Error(w, "quantity must be greater than 0", http.StatusBadRequest)
		return
	}

	if err := h.store.UpsertCartItem(r.Context(), userID, itemID, req.Quantity); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

func (h *Handler) RemoveCartItem(w http.ResponseWriter, r *http.Request) {
	// TODO: Protect this method
	userID := r.Context().Value(UserContextKey).(int)

	itemIDStr := r.PathValue("item_id")
	if itemIDStr == "" {
		http.Error(w, "missing item_id", http.StatusBadRequest)
		return
	}

	itemID, err := strconv.Atoi(itemIDStr)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
			Message: "item_id must be integer",
		})
		return
	}

	if err := h.store.RemoveCartItem(r.Context(), userID, itemID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) GetUserCart(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(UserContextKey).(int)

	cartItems, err := h.store.GetUserCart(r.Context(), userID)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// TODO: instead of returning Cart
	// return CartResponse instead
	// tip: refactor the GetUserCart method

	writeJSON(w, http.StatusOK, models.CartResponse{
		UserID: userID,
		Items:  cartItems,
	})
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	// TODO: protect this method
	userID := r.Context().Value(UserContextKey).(int)

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
		return
	}

	if record, err := h.store.FindIdempotencyKey(r.Context(), idempotencyKey); err == nil {
		if time.Now().Before(record.Expiry) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(record.StatusCode)
			w.Write(record.Response)
			return
		}
		err := h.store.DeleteIdempotencyKey(r.Context(), idempotencyKey)
		if err != nil {
			http.Error(w, "failed to delete idempotency key", http.StatusInternalServerError)
			return
		}
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.LineItems) == 0 {
		http.Error(w, "items must not be empty", http.StatusBadRequest)
		return
	}

	items := make([]models.LineItem, 0, len(req.LineItems))
	for _, i := range req.LineItems {
		items = append(items, models.LineItem{
			ItemID:   i.ItemID,
			Quantity: i.Quantity,
			Price:    i.Price,
		})
	}

	order, err := h.store.CreateOrder(r.Context(), userID, items, req.Total, "pending")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	paymentResult := mockProcessPayment(req.Total)

	status := "paid"
	if !paymentResult.Success {
		status = "failed"
	}

	if err := h.store.UpdateOrderStatus(r.Context(), order.ID, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	order.Status = status

	responseData := map[string]any{
		"order":   order,
		"payment": paymentResult,
	}

	statusCode := http.StatusCreated
	if !paymentResult.Success {
		statusCode = http.StatusPaymentRequired
	}

	responseBody, _ := json.Marshal(responseData)
	err = h.store.SaveIdempotencyKey(r.Context(), idempotencyKey, statusCode, time.Now().Add(24*time.Hour), responseBody)
	if err != nil {
		http.Error(w, "failed to save idempotency key", http.StatusInternalServerError)
		return
	}

	writeJSON(w, statusCode, responseData)
}

// GetItems handles GET /items — returns all available items.
func (h *Handler) GetItems(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.GetItems(r.Context())
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, items)
	return
}

// GetItemByID handles GET /items/{id} — returns a single i`tem.
func (h *Handler) GetItemByID(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	itemIDStr := parts[2] // /items/1 → parts[2] = "1"

	itemID, err := strconv.Atoi(itemIDStr)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
			Message: "item_id must be an integer",
		})
		return
	}

	item, err := h.store.GetItem(r.Context(), itemID)
	if err != nil {
		fmt.Printf("error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if item == nil {
		writeJSON(w, http.StatusNotFound, nil)
		return
	}

	writeJSON(w, http.StatusOK, item)
	return
}

func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	// validate email
	_, err = mail.ParseAddress(req.Email)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
			Message: "invalid email",
		})
		return
	}

	pwlen := len(req.Password)
	// validate password
	if pwlen < 12 || pwlen > 25 {
		writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
			Message: "password is too short or too long",
		})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	err = h.store.SaveUser(r.Context(), req.Email, hash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" { // unique_violation
				writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
					Message: "email already exists",
				})
				return
			}
		}
		fmt.Printf("CreateUser DB error: %v\n", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"message": "success",
	})
}

func (h *Handler) LoginUser(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		fmt.Println("Failed to decode body:", err)
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	user, err := h.store.FindUserByEmail(r.Context(), req.Email)
	if err != nil {
		fmt.Println("FindUserByEmail error:", err)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusUnprocessableEntity, ErrorMessageResponse{
				Message: "user does not exist",
			})
			return
		}
		fmt.Println("FindUserByEmail error:", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	fmt.Println("User found:", user.Email, "Hash length:", len(user.Hash))

	// Compare password with stored hash
	err = bcrypt.CompareHashAndPassword(user.Hash, []byte(req.Password))
	if err != nil {
		fmt.Println("Password mismatch or hash error:", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// ✅ Password verified, now generate JWT + refresh token

	// 1. Generate JWT
	expiration := time.Now().Add(15 * time.Minute)

	claims := MyCustomClaims{
		Email: user.Email, // Add the email here
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.Itoa(user.ID),
			ExpiresAt: jwt.NewNumericDate(expiration),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	jwtToken, err := token.SignedString([]byte(SigningSecret))
	if err != nil {
		http.Error(w, "could not generate token", http.StatusInternalServerError)
		return
	}

	// 2. Generate refresh token
	refreshToken, err := generateRefreshToken()
	if err != nil {
		http.Error(w, "could not generate refresh token", http.StatusInternalServerError)
		return
	}

	// 3. Save refresh token in DB
	err = h.store.SaveRefreshToken(r.Context(), user.ID, refreshToken)
	if err != nil {
		http.Error(w, "could not save refresh token", http.StatusInternalServerError)
		return
	}

	// 4. Return JWT + refresh token
	writeJSON(w, http.StatusOK, AuthResponse{
		JWT:          jwtToken,
		RefreshToken: refreshToken,
	})
}

func (h *Handler) IssueJWT(w http.ResponseWriter, r *http.Request) {
	// TODO: implement issueing of new JWT with refresh token
	// check if refresh_token exists in the db and still active
	// generate a new JWT
	// generate a new random string (bonus: if you use a CSPRNG to generate a random sequence of bytes) as refresh_token
	// save new refresh token in db
	// deactivate old refresh token

	var req RefreshTokenRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	refreshToken := req.RefreshToken

	// 1. Check refresh token in DB
	userID, err := h.store.ValidateRefreshToken(r.Context(), refreshToken)
	if err != nil {
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}

	user, err := h.store.FindUserByID(r.Context(), userID)
	if err != nil {
		http.Error(w, "user not found", http.StatusUnauthorized)
		return
	}

	// 2. Generate new JWT
	expiration := time.Now().Add(15 * time.Minute)
	claims := MyCustomClaims{
		Email: user.Email, // This now works because 'user' is a struct
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.Itoa(userID),
			ExpiresAt: jwt.NewNumericDate(expiration),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	jwtToken, err := token.SignedString([]byte(SigningSecret))
	if err != nil {
		http.Error(w, "could not generate token", http.StatusInternalServerError)
		return
	}

	// 3. Generate new refresh token
	newRefreshToken, err := generateRefreshToken()
	if err != nil {
		http.Error(w, "could not generate refresh token", http.StatusInternalServerError)
		return
	}

	// 4. Save new refresh token
	err = h.store.SaveRefreshToken(r.Context(), userID, newRefreshToken)
	if err != nil {
		http.Error(w, "could not save refresh token", http.StatusInternalServerError)
		return
	}

	// 5. Deactivate old refresh token
	err = h.store.DeactivateRefreshToken(r.Context(), refreshToken)
	if err != nil {
		http.Error(w, "could not deactivate refresh token", http.StatusInternalServerError)
		return
	}

	// 6. Return tokens
	writeJSON(w, http.StatusOK, AuthResponse{
		JWT:          jwtToken,
		RefreshToken: newRefreshToken,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
