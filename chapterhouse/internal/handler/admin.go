package handler

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/logan-broit/ghola/chapterhouse/internal/auth"
	"github.com/logan-broit/ghola/chapterhouse/internal/repository/sqlc"
	"github.com/logan-broit/ghola/chapterhouse/pkg/apierror"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// getIPAddress extracts the IP address from a request, stripping the port.
func getIPAddress(r *http.Request) string {
	// Check X-Forwarded-For header first (for proxied requests)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs; take the first one
		if idx := len(xff); idx > 0 {
			for i, c := range xff {
				if c == ',' {
					return xff[:i]
				}
			}
			return xff
		}
	}

	// Fall back to RemoteAddr, stripping the port
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr might not have a port (unlikely but handle it)
		return r.RemoteAddr
	}
	return host
}

// AdminHandler serves account (/v1/me, /v1/keys, /v1/audit) and admin
// (/v1/admin/*) routes. Callers authenticate with an API key.
type AdminHandler struct {
	queries sqlc.Querier
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(queries sqlc.Querier) *AdminHandler {
	return &AdminHandler{queries: queries}
}

// GetCurrentUser handles GET /v1/me
// Returns the currently authenticated user's information.
func (h *AdminHandler) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	authCtx := auth.FromContext(r.Context())
	if authCtx == nil {
		Error(w, r, apierror.Unauthorized("Not authenticated"))
		return
	}

	user, err := h.queries.GetUserByID(r.Context(), authCtx.UserID)
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to get user").WithError(err))
		return
	}

	OK(w, toUserResponse(user))
}

// UserResponse represents a user in API responses.
type UserResponse struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	Email         string     `json:"email,omitempty"`
	DisplayName   string     `json:"display_name,omitempty"`
	IsAdmin       bool       `json:"is_admin"`
	CreatedAt     time.Time  `json:"created_at"`
	ModifiedAt    time.Time  `json:"modified_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

func toUserResponse(u sqlc.User) UserResponse {
	resp := UserResponse{
		ID:          u.ID.String(),
		Username:    u.Username,
		Email:       u.Email.String,
		DisplayName: u.DisplayName.String,
		IsAdmin:     u.IsAdmin,
		CreatedAt:   u.CreatedAt,
		ModifiedAt:  u.ModifiedAt,
	}
	if u.DeactivatedAt.Valid {
		resp.DeactivatedAt = &u.DeactivatedAt.Time
	}
	return resp
}

// ListUsers handles GET /v1/admin/users
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset := parseAdminPagination(r, 50, 1000)
	activeOnly := r.URL.Query().Get("active") == "true"

	users, err := h.queries.ListUsersAdmin(r.Context(), sqlc.ListUsersAdminParams{
		Limit:   limit,
		Offset:  offset,
		Column3: activeOnly,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list users").WithError(err))
		return
	}

	result := make([]UserResponse, len(users))
	for i, u := range users {
		result[i] = toUserResponse(u)
	}

	// Get total count for pagination
	counts, _ := h.queries.CountUsers(r.Context())

	OK(w, NewPaginatedResponse(result, int(offset), int(limit), int(counts.Total)))
}

// GetUser handles GET /v1/admin/users/{id}
func (h *AdminHandler) GetUser(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	user, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		Error(w, r, dbError(err, "User", "get user"))
		return
	}

	OK(w, toUserResponse(user))
}

// CreateUserRequest represents a create user request.
type CreateUserRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	IsAdmin     bool   `json:"is_admin"`
}

// CreateUser handles POST /v1/admin/users
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, r, err)
		return
	}

	if req.Username == "" {
		Error(w, r, apierror.BadRequest("Username is required"))
		return
	}

	// Check if username already exists
	_, err := h.queries.GetUserByUsername(r.Context(), req.Username)
	if err == nil {
		Error(w, r, apierror.Conflict("Username already exists"))
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, apierror.InternalError("Failed to check username").WithError(err))
		return
	}

	user, err := h.queries.CreateUserWithPassword(r.Context(), sqlc.CreateUserWithPasswordParams{
		ID:           uuid.New(),
		Username:     req.Username,
		Email:        pgtype.Text{String: req.Email, Valid: req.Email != ""},
		DisplayName:  pgtype.Text{String: req.DisplayName, Valid: req.DisplayName != ""},
		PasswordHash: pgtype.Text{},
		IsAdmin:      req.IsAdmin,
		Metadata:     []byte("{}"),
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to create user").WithError(err))
		return
	}

	// Log audit event
	authCtx := auth.FromContext(r.Context())
	if authCtx != nil {
		_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
			UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
			Action:       "create",
			ResourceType: "user",
			ResourceID:   pgtype.Text{String: user.ID.String(), Valid: true},
			Details:      nil,
			IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
			UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
		})
	}

	Created(w, toUserResponse(user))
}

// UpdateUserRequest represents an update user request.
type UpdateUserRequest struct {
	Username    string  `json:"username,omitempty"`
	Email       *string `json:"email,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	IsAdmin     *bool   `json:"is_admin,omitempty"`
}

// UpdateUser handles PUT /v1/admin/users/{id}
func (h *AdminHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	var req UpdateUserRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, r, err)
		return
	}

	existing, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		Error(w, r, dbError(err, "User", "get user"))
		return
	}

	username := existing.Username
	if req.Username != "" {
		username = req.Username
	}

	email := existing.Email
	if req.Email != nil {
		email = pgtype.Text{String: *req.Email, Valid: *req.Email != ""}
	}

	displayName := existing.DisplayName
	if req.DisplayName != nil {
		displayName = pgtype.Text{String: *req.DisplayName, Valid: *req.DisplayName != ""}
	}

	isAdmin := existing.IsAdmin
	if req.IsAdmin != nil {
		isAdmin = *req.IsAdmin
	}

	user, err := h.queries.UpdateUserAdmin(r.Context(), sqlc.UpdateUserAdminParams{
		ID:          userID,
		Username:    username,
		Email:       email,
		DisplayName: displayName,
		IsAdmin:     isAdmin,
		Metadata:    existing.Metadata,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to update user").WithError(err))
		return
	}

	// Log audit event
	authCtx := auth.FromContext(r.Context())
	if authCtx != nil {
		_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
			UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
			Action:       "update",
			ResourceType: "user",
			ResourceID:   pgtype.Text{String: user.ID.String(), Valid: true},
			Details:      nil,
			IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
			UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
		})
	}

	OK(w, toUserResponse(user))
}

// DeactivateUser handles DELETE /v1/admin/users/{id}
func (h *AdminHandler) DeactivateUser(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	_, err = h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		Error(w, r, dbError(err, "User", "get user"))
		return
	}

	if err := h.queries.DeactivateUser(r.Context(), userID); err != nil {
		Error(w, r, apierror.InternalError("Failed to deactivate user").WithError(err))
		return
	}

	_ = h.queries.RevokeAllAPIKeysByUser(r.Context(), userID)

	// Log audit event
	authCtx := auth.FromContext(r.Context())
	if authCtx != nil {
		_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
			UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
			Action:       "delete",
			ResourceType: "user",
			ResourceID:   pgtype.Text{String: userID.String(), Valid: true},
			Details:      nil,
			IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
			UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
		})
	}

	NoContent(w)
}

// ReactivateUser handles POST /v1/admin/users/{id}/reactivate
func (h *AdminHandler) ReactivateUser(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	if err := h.queries.ReactivateUser(r.Context(), userID); err != nil {
		Error(w, r, apierror.InternalError("Failed to reactivate user").WithError(err))
		return
	}

	// Return the updated user
	user, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to get user").WithError(err))
		return
	}

	OK(w, toUserResponse(user))
}

// APIKeyResponse represents an API key in responses (never includes the actual key).
type APIKeyResponse struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Username   string     `json:"username,omitempty"`
	KeyPrefix  string     `json:"key_prefix"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func newAPIKeyResponse(id, userID uuid.UUID, username, keyPrefix, name string, createdAt time.Time, lastUsedAt, expiresAt, revokedAt pgtype.Timestamptz) APIKeyResponse {
	resp := APIKeyResponse{
		ID:        id.String(),
		UserID:    userID.String(),
		Username:  username,
		KeyPrefix: keyPrefix,
		Name:      name,
		CreatedAt: createdAt,
	}
	if lastUsedAt.Valid {
		resp.LastUsedAt = &lastUsedAt.Time
	}
	if expiresAt.Valid {
		resp.ExpiresAt = &expiresAt.Time
	}
	if revokedAt.Valid {
		resp.RevokedAt = &revokedAt.Time
	}
	return resp
}

func toAPIKeyResponseFromUserRow(k sqlc.ListAPIKeysByUserRow) APIKeyResponse {
	return newAPIKeyResponse(k.ID, k.UserID, k.Username, k.KeyPrefix, k.Name, k.CreatedAt, k.LastUsedAt, k.ExpiresAt, k.RevokedAt)
}

func toAPIKeyResponseFromAllRow(k sqlc.ListAllAPIKeysRow) APIKeyResponse {
	return newAPIKeyResponse(k.ID, k.UserID, k.Username, k.KeyPrefix, k.Name, k.CreatedAt, k.LastUsedAt, k.ExpiresAt, k.RevokedAt)
}

func toAPIKeyResponse(k sqlc.ApiKey) APIKeyResponse {
	return newAPIKeyResponse(k.ID, k.UserID, "", k.KeyPrefix, k.Name, k.CreatedAt, k.LastUsedAt, k.ExpiresAt, k.RevokedAt)
}

// ListAPIKeys handles GET /v1/admin/users/{id}/keys
func (h *AdminHandler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	keys, err := h.queries.ListAPIKeysByUser(r.Context(), userID)
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list API keys").WithError(err))
		return
	}

	result := make([]APIKeyResponse, len(keys))
	for i, k := range keys {
		result[i] = toAPIKeyResponseFromUserRow(k)
	}

	OK(w, map[string]interface{}{
		"keys": result,
	})
}

// ListAllAPIKeys handles GET /v1/admin/keys
func (h *AdminHandler) ListAllAPIKeys(w http.ResponseWriter, r *http.Request) {
	limit, offset := parseAdminPagination(r, 100, 1000)

	keys, err := h.queries.ListAllAPIKeys(r.Context(), sqlc.ListAllAPIKeysParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list API keys").WithError(err))
		return
	}

	result := make([]APIKeyResponse, len(keys))
	for i, k := range keys {
		result[i] = toAPIKeyResponseFromAllRow(k)
	}

	OK(w, map[string]interface{}{
		"keys": result,
	})
}

// CreateAPIKeyRequest represents a create API key request.
type CreateAPIKeyRequest struct {
	Name      string `json:"name"`
	ExpiresIn string `json:"expires_in,omitempty"` // e.g., "30d", "90d", "1y"
}

// CreateAPIKeyResponse includes the plaintext key (shown only once).
type CreateAPIKeyResponse struct {
	APIKeyResponse
	Key string `json:"key"` // The plaintext key - shown only once!
}

// CreateAPIKey handles POST /v1/admin/users/{id}/keys
func (h *AdminHandler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid user ID"))
		return
	}

	_, err = h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		Error(w, r, dbError(err, "User", "get user"))
		return
	}

	var req CreateAPIKeyRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, r, err)
		return
	}

	if req.Name == "" {
		Error(w, r, apierror.BadRequest("Name is required"))
		return
	}

	// Generate the API key
	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to generate API key").WithError(err))
		return
	}

	// Parse expiration
	var expiresAt pgtype.Timestamptz
	if req.ExpiresIn != "" {
		duration, err := parseDuration(req.ExpiresIn)
		if err != nil {
			Error(w, r, apierror.BadRequest("Invalid expires_in format. Use: 30d, 90d, 1y, etc."))
			return
		}
		expiresAt = pgtype.Timestamptz{Time: time.Now().Add(duration), Valid: true}
	}

	// Store the key
	apiKey, err := h.queries.CreateAPIKey(r.Context(), sqlc.CreateAPIKeyParams{
		UserID:    userID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      req.Name,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to create API key").WithError(err))
		return
	}

	// Log audit event
	authCtx := auth.FromContext(r.Context())
	if authCtx != nil {
		_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
			UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
			Action:       "create",
			ResourceType: "api_key",
			ResourceID:   pgtype.Text{String: apiKey.ID.String(), Valid: true},
			Details:      nil,
			IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
			UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
		})
	}

	// Return the response with plaintext key
	resp := CreateAPIKeyResponse{
		APIKeyResponse: toAPIKeyResponse(apiKey),
		Key:            plaintext,
	}

	Created(w, resp)
}

// RevokeAPIKey handles DELETE /v1/admin/keys/{id}
func (h *AdminHandler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid API key ID"))
		return
	}

	_, err = h.queries.GetAPIKeyByID(r.Context(), keyID)
	if err != nil {
		Error(w, r, dbError(err, "API key", "get API key"))
		return
	}

	if err := h.queries.RevokeAPIKey(r.Context(), keyID); err != nil {
		Error(w, r, apierror.InternalError("Failed to revoke API key").WithError(err))
		return
	}

	// Log audit event
	authCtx := auth.FromContext(r.Context())
	if authCtx != nil {
		_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
			UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
			Action:       "revoke",
			ResourceType: "api_key",
			ResourceID:   pgtype.Text{String: keyID.String(), Valid: true},
			Details:      nil,
			IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
			UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
		})
	}

	NoContent(w)
}

// StatsResponse represents admin statistics.
type StatsResponse struct {
	ActiveUsers   int64 `json:"active_users"`
	AdminUsers    int64 `json:"admin_users"`
	ActiveAPIKeys int64 `json:"active_api_keys"`
}

// GetStats handles GET /v1/admin/stats
func (h *AdminHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.queries.GetAdminStats(r.Context())
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to get stats").WithError(err))
		return
	}

	OK(w, StatsResponse{
		ActiveUsers:   stats.ActiveUsers,
		AdminUsers:    stats.AdminUsers,
		ActiveAPIKeys: stats.ActiveApiKeys,
	})
}

// AuditLogResponse represents an audit log entry.
type AuditLogResponse struct {
	ID             string          `json:"id"`
	ActorID        string          `json:"actor_id,omitempty"`
	ActorUsername  string          `json:"actor_username,omitempty"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id,omitempty"`
	TargetUsername string          `json:"target_username,omitempty"` // Username of user affected by action (e.g., API key owner)
	Details        json.RawMessage `json:"details,omitempty"`
	IPAddress      string          `json:"ip_address,omitempty"`
	UserAgent      string          `json:"user_agent,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// AuditListResponse wraps a list of audit log entries with pagination info.
type AuditListResponse struct {
	Entries []AuditLogResponse `json:"entries"`
	Total   int                `json:"total"`
}

// ListAuditLogs handles GET /v1/admin/audit
func (h *AdminHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit, offset := parseAdminPagination(r, 50, 1000)

	// Parse optional filter parameters
	var userID uuid.UUID
	if uid := r.URL.Query().Get("user_id"); uid != "" {
		if parsed, err := uuid.Parse(uid); err == nil {
			userID = parsed
		}
	}
	action := r.URL.Query().Get("action")
	resourceType := r.URL.Query().Get("resource_type")

	// Get total count for pagination
	totalCount, err := h.queries.CountAuditLogs(r.Context(), sqlc.CountAuditLogsParams{
		Column1: userID,       // user_id filter (nil UUID = no filter)
		Column2: action,       // action filter (empty string = no filter)
		Column3: resourceType, // resource_type filter (empty string = no filter)
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to count audit logs").WithError(err))
		return
	}

	logs, err := h.queries.ListAuditLogs(r.Context(), sqlc.ListAuditLogsParams{
		Column1: userID,       // user_id filter (nil UUID = no filter)
		Column2: action,       // action filter (empty string = no filter)
		Column3: resourceType, // resource_type filter (empty string = no filter)
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list audit logs").WithError(err))
		return
	}

	// Collect unique user IDs to fetch usernames (for actors)
	userIDMap := make(map[uuid.UUID]bool)
	for _, l := range logs {
		if l.UserID.Valid {
			userUUID, _ := uuid.FromBytes(l.UserID.Bytes[:])
			userIDMap[userUUID] = true
		}
	}

	// Fetch usernames for all unique user IDs
	usernameCache := make(map[uuid.UUID]string)
	for uid := range userIDMap {
		user, err := h.queries.GetUserByID(r.Context(), uid)
		if err == nil {
			usernameCache[uid] = user.Username
		}
	}

	// Collect API key IDs to fetch their associated users (for target usernames)
	apiKeyIDMap := make(map[uuid.UUID]bool)
	for _, l := range logs {
		if l.ResourceType == "api_key" && l.ResourceID.Valid && l.ResourceID.String != "" {
			if keyID, err := uuid.Parse(l.ResourceID.String); err == nil {
				apiKeyIDMap[keyID] = true
			}
		}
	}

	// Fetch API key owners
	apiKeyOwnerCache := make(map[uuid.UUID]string) // maps api_key_id -> username
	for keyID := range apiKeyIDMap {
		apiKey, err := h.queries.GetAPIKeyByID(r.Context(), keyID)
		if err == nil {
			// Get the username for this user_id
			user, err := h.queries.GetUserByID(r.Context(), apiKey.UserID)
			if err == nil {
				apiKeyOwnerCache[keyID] = user.Username
			}
		}
	}

	entries := make([]AuditLogResponse, len(logs))
	for i, l := range logs {
		resp := AuditLogResponse{
			ID:           l.Guid.String(),
			Action:       l.Action,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID.String,
			Details:      l.Details,
			IPAddress:    l.IpAddress.String,
			UserAgent:    l.UserAgent.String,
			CreatedAt:    l.CreatedAt,
		}
		if l.UserID.Valid {
			userUUID, _ := uuid.FromBytes(l.UserID.Bytes[:])
			resp.ActorID = userUUID.String()
			if username, ok := usernameCache[userUUID]; ok {
				resp.ActorUsername = username
			}
		}

		// For API key operations, include the username of the key owner
		if l.ResourceType == "api_key" && l.ResourceID.Valid && l.ResourceID.String != "" {
			if keyID, err := uuid.Parse(l.ResourceID.String); err == nil {
				if ownerUsername, ok := apiKeyOwnerCache[keyID]; ok {
					resp.TargetUsername = ownerUsername
				}
			}
		}

		entries[i] = resp
	}

	OK(w, AuditListResponse{
		Entries: entries,
		Total:   int(totalCount),
	})
}

// ListUserAPIKeys handles GET /v1/keys
// Returns only API keys for the authenticated user.
func (h *AdminHandler) ListUserAPIKeys(w http.ResponseWriter, r *http.Request) {
	authCtx := auth.FromContext(r.Context())
	if authCtx == nil {
		Error(w, r, apierror.Unauthorized("Authentication required"))
		return
	}

	keys, err := h.queries.ListAPIKeysByUser(r.Context(), authCtx.UserID)
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list API keys").WithError(err))
		return
	}

	result := make([]APIKeyResponse, len(keys))
	for i, k := range keys {
		result[i] = toAPIKeyResponseFromUserRow(k)
	}

	OK(w, map[string]interface{}{
		"keys": result,
	})
}

// CreateUserAPIKey handles POST /v1/keys
// Creates an API key for the authenticated user.
func (h *AdminHandler) CreateUserAPIKey(w http.ResponseWriter, r *http.Request) {
	authCtx := auth.FromContext(r.Context())
	if authCtx == nil {
		Error(w, r, apierror.Unauthorized("Authentication required"))
		return
	}

	var req CreateAPIKeyRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, r, err)
		return
	}

	if req.Name == "" {
		Error(w, r, apierror.BadRequest("Name is required"))
		return
	}

	// Generate the API key
	plaintext, hash, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to generate API key").WithError(err))
		return
	}

	// Parse expiration
	var expiresAt pgtype.Timestamptz
	if req.ExpiresIn != "" {
		duration, err := parseDuration(req.ExpiresIn)
		if err != nil {
			Error(w, r, apierror.BadRequest("Invalid expires_in format. Use: 30d, 90d, 1y, etc."))
			return
		}
		expiresAt = pgtype.Timestamptz{Time: time.Now().Add(duration), Valid: true}
	}

	// Store the key for the current user
	apiKey, err := h.queries.CreateAPIKey(r.Context(), sqlc.CreateAPIKeyParams{
		UserID:    authCtx.UserID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      req.Name,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to create API key").WithError(err))
		return
	}

	_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
		UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
		Action:       "create",
		ResourceType: "api_key",
		ResourceID:   pgtype.Text{String: apiKey.ID.String(), Valid: true},
		Details:      nil,
		IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
		UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
	})

	// Return the response with plaintext key
	resp := CreateAPIKeyResponse{
		APIKeyResponse: toAPIKeyResponse(apiKey),
		Key:            plaintext,
	}

	Created(w, resp)
}

// RevokeUserAPIKey handles DELETE /v1/keys/{id}
// Revokes an API key owned by the authenticated user.
func (h *AdminHandler) RevokeUserAPIKey(w http.ResponseWriter, r *http.Request) {
	authCtx := auth.FromContext(r.Context())
	if authCtx == nil {
		Error(w, r, apierror.Unauthorized("Authentication required"))
		return
	}

	keyID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		Error(w, r, apierror.BadRequest("Invalid API key ID"))
		return
	}

	apiKey, err := h.queries.GetAPIKeyByID(r.Context(), keyID)
	if err != nil {
		Error(w, r, dbError(err, "API key", "get API key"))
		return
	}

	// Verify ownership
	if apiKey.UserID != authCtx.UserID {
		Error(w, r, apierror.Forbidden("You can only revoke your own API keys"))
		return
	}

	if err := h.queries.RevokeAPIKey(r.Context(), keyID); err != nil {
		Error(w, r, apierror.InternalError("Failed to revoke API key").WithError(err))
		return
	}

	_, _ = h.queries.CreateAuditLog(r.Context(), sqlc.CreateAuditLogParams{
		UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
		Action:       "revoke",
		ResourceType: "api_key",
		ResourceID:   pgtype.Text{String: keyID.String(), Valid: true},
		Details:      nil,
		IpAddress:    pgtype.Text{String: getIPAddress(r), Valid: true},
		UserAgent:    pgtype.Text{String: r.UserAgent(), Valid: true},
	})

	NoContent(w)
}

// ListUserAuditLogs handles GET /v1/audit
// Returns only audit log entries for the authenticated user (no user_id filter allowed)
func (h *AdminHandler) ListUserAuditLogs(w http.ResponseWriter, r *http.Request) {
	// Get the authenticated user from context
	authCtx := auth.FromContext(r.Context())
	if authCtx == nil {
		Error(w, r, apierror.Unauthorized("Authentication required"))
		return
	}

	limit, offset := parseAdminPagination(r, 50, 1000)

	// Parse optional filter parameters (but NOT user_id - that's fixed to current user)
	action := r.URL.Query().Get("action")
	resourceType := r.URL.Query().Get("resource_type")

	// Get total count for pagination (scoped to current user)
	totalCount, err := h.queries.CountAuditLogs(r.Context(), sqlc.CountAuditLogsParams{
		Column1: authCtx.UserID, // Always filter to current user
		Column2: action,
		Column3: resourceType,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to count audit logs").WithError(err))
		return
	}

	logs, err := h.queries.ListAuditLogs(r.Context(), sqlc.ListAuditLogsParams{
		Column1: authCtx.UserID, // Always filter to current user
		Column2: action,
		Column3: resourceType,
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		Error(w, r, apierror.InternalError("Failed to list audit logs").WithError(err))
		return
	}

	// Build response entries
	entries := make([]AuditLogResponse, len(logs))
	for i, l := range logs {
		resp := AuditLogResponse{
			ID:           l.Guid.String(),
			Action:       l.Action,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID.String,
			Details:      l.Details,
			IPAddress:    l.IpAddress.String,
			UserAgent:    l.UserAgent.String,
			CreatedAt:    l.CreatedAt,
		}
		if l.UserID.Valid {
			userUUID, _ := uuid.FromBytes(l.UserID.Bytes[:])
			resp.ActorID = userUUID.String()
			resp.ActorUsername = authCtx.Username // We know it's the current user
		}
		entries[i] = resp
	}

	OK(w, AuditListResponse{
		Entries: entries,
		Total:   int(totalCount),
	})
}

func parseAdminPagination(r *http.Request, defaultLimit, maxLimit int32) (limit, offset int32) {
	limit = defaultLimit
	offset = 0

	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.ParseInt(l, 10, 32); err == nil && parsed > 0 {
			limit = int32(parsed)
			if limit > maxLimit {
				limit = maxLimit
			}
		}
	}

	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.ParseInt(o, 10, 32); err == nil && parsed >= 0 {
			offset = int32(parsed)
		}
	}

	return limit, offset
}

func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, errors.New("invalid duration")
	}

	numStr := s[:len(s)-1]
	unit := s[len(s)-1]

	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 {
		return 0, errors.New("invalid duration number")
	}

	switch unit {
	case 'd':
		return time.Duration(num) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(num) * 7 * 24 * time.Hour, nil
	case 'm':
		return time.Duration(num) * 30 * 24 * time.Hour, nil
	case 'y':
		return time.Duration(num) * 365 * 24 * time.Hour, nil
	default:
		return 0, errors.New("invalid duration unit")
	}
}
