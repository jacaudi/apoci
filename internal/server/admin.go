package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/admin"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

const adminMaxBody int64 = 4 * 1024 // 4 KB

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(bearerAuthMiddleware(s.cfg.AdminToken))

	r.Get("/identity", s.adminGetIdentity)
	r.Get("/images", s.adminListImages)
	r.Get("/actors", s.adminListActors)
	r.Get("/follows", s.adminListFollows)
	r.Get("/follows/pending", s.adminListPending)
	r.Get("/follows/outgoing", s.adminListOutgoingFollows)
	r.Post("/follows", s.adminAddFollow)
	r.Post("/follows/accept", s.adminAcceptFollow)
	r.Post("/follows/reject", s.adminRejectFollow)
	r.Delete("/follows", s.adminRemoveFollow)
	r.Patch("/follows", s.adminUpdateFollowFilter)
	r.Delete("/mirrors/*", s.adminEvictMirror)
	r.Get("/gc", s.adminGCStatus)
	r.Post("/gc", s.adminRunGC)

	return r
}

func (s *Server) adminGetIdentity(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /identity")
	pubPEM, err := s.identity.PublicKeyPEM()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{
		"name":          s.cfg.Name,
		"actorURL":      s.identity.ActorURL,
		"keyID":         s.identity.KeyID(),
		"domain":        s.identity.Domain,
		"accountDomain": s.identity.AccountDomain,
		"endpoint":      s.cfg.Endpoint,
		"publicKey":     pubPEM,
	})
}

func (s *Server) adminListImages(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /images")
	repos, err := s.db.ListLocallyHostedRepos(r.Context())
	if err != nil {
		s.logger.Error("listing locally hosted repos", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entries := make([]admin.ImageEntry, len(repos))
	for i, repo := range repos {
		entries[i] = admin.ImageEntry{
			Name:      repo.Name,
			Tags:      repo.Tags,
			SizeBytes: repo.SizeBytes,
			UpdatedAt: repo.UpdatedAt,
		}
	}
	s.logger.Debug("admin: GET /images done", "count", len(entries))
	writeJSON(w, entries)
}

func (s *Server) adminListActors(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /actors")
	actors, err := s.db.ListActors(r.Context())
	if err != nil {
		s.logger.Error("listing actors", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /actors done", "count", len(actors))
	writeJSON(w, actors)
}

func (s *Server) adminListFollows(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /follows")
	follows, err := s.db.ListFollows(r.Context())
	if err != nil {
		s.logger.Error("listing follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows done", "count", len(follows))
	writeJSON(w, follows)
}

func (s *Server) adminListPending(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /follows/pending")
	requests, err := s.db.ListFollowRequests(r.Context())
	if err != nil {
		s.logger.Error("listing pending requests", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows/pending done", "count", len(requests))
	writeJSON(w, requests)
}

func (s *Server) adminListOutgoingFollows(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	s.logger.Debug("admin: GET /follows/outgoing", "status", status)
	var follows []database.Actor
	var err error

	if status != "" {
		follows, err = s.db.ListOutgoingFollows(r.Context(), status)
	} else {
		follows, err = s.db.ListAllOutgoingFollows(r.Context())
	}
	if err != nil {
		s.logger.Error("listing outgoing follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows/outgoing done", "count", len(follows))
	writeJSON(w, follows)
}

type adminFollowRequest struct {
	Target string `json:"target"`
	Force  bool   `json:"force,omitempty"`
}

// decodeTarget reads the follow request body and returns the raw target string.
func decodeTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return "", false
	}
	return req.Target, true
}

func (s *Server) adminAddFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows", "target", target)

	result, err := s.fedSvc.AddFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("adding follow", "target", target, "error", err)
		http.Error(w, "could not add follow", classifyError(err))
		return
	}
	s.logger.Debug("admin: POST /follows done", "target", target, "actorID", result.ActorID)

	writeJSON(w, map[string]string{"followed": result.ActorID})
}

func (s *Server) adminAcceptFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows/accept", "target", target)

	result, err := s.fedSvc.AcceptFollow(r.Context(), target, s.cfg.Federation.AutoAccept)
	if err != nil {
		s.logger.Error("accepting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: POST /follows/accept done", "actorURL", result.ActorURL, "followedBack", result.FollowedBack)

	resp := map[string]string{"accepted": result.ActorURL}
	if result.FollowedBack {
		resp["followed_back"] = result.ActorURL
	}
	writeJSON(w, resp)
}

func (s *Server) adminRejectFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows/reject", "target", target)

	actorURL, err := s.fedSvc.RejectFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("rejecting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: POST /follows/reject done", "actorURL", actorURL)

	writeJSON(w, map[string]string{"rejected": actorURL})
}

func (s *Server) adminRemoveFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}
	s.logger.Debug("admin: DELETE /follows", "target", req.Target, "force", req.Force)

	actorURL, err := s.fedSvc.RemoveFollow(r.Context(), req.Target, req.Force)
	if err != nil {
		s.logger.Error("removing follow", "target", req.Target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: DELETE /follows done", "actorURL", actorURL)

	writeJSON(w, map[string]string{"removed": actorURL})
}

type adminFollowFilterRequest struct {
	Target   string   `json:"target"`
	TagGlobs []string `json:"tag_globs"`
}

func (s *Server) adminUpdateFollowFilter(w http.ResponseWriter, r *http.Request) {
	var req adminFollowFilterRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}
	s.logger.Debug("admin: PATCH /follows", "target", req.Target, "tag_globs", req.TagGlobs)

	if err := s.db.UpdateFollowFilter(r.Context(), req.Target, req.TagGlobs); err != nil {
		s.logger.Error("updating follow filter", "target", req.Target, "error", err)
		switch {
		case errors.Is(err, database.ErrInvalidGlob):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, database.ErrFollowerNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, map[string]any{"updated": req.Target, "tag_globs": req.TagGlobs})
}

func (s *Server) adminEvictMirror(w http.ResponseWriter, r *http.Request) {
	repo := chi.URLParam(r, "*")
	if repo == "" {
		http.Error(w, "missing repository", http.StatusBadRequest)
		return
	}
	if err := validate.RepoName(repo); err != nil {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}
	digest := r.URL.Query().Get("digest")
	if digest != "" {
		if err := validate.Digest(digest); err != nil {
			http.Error(w, "invalid digest", http.StatusBadRequest)
			return
		}
	}
	s.logger.Debug("admin: DELETE /mirrors", "repo", repo, "digest", digest)

	repoObj, err := s.db.GetRepository(r.Context(), repo)
	if err != nil {
		s.logger.Error("looking up repo for eviction", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if repoObj == nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if repoObj.OwnerID == s.identity.ActorURL {
		http.Error(w, "repository is locally owned; use the /v2/ delete API to remove it", http.StatusBadRequest)
		return
	}

	if digest != "" {
		purged, err := s.db.DeletePackageVersionWithBlobs(r.Context(), repoObj.ID, digest)
		if err != nil {
			s.logger.Error("evicting mirror manifest", "repo", repo, "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, d := range purged {
			if err := s.blobs.Delete(r.Context(), d); err != nil {
				s.logger.Warn("evict: failed to delete blob bytes", "digest", d, "error", err)
			}
		}
		writeJSON(w, map[string]any{"evicted": repo, "digest": digest, "blobsPurged": len(purged)})
		return
	}

	purged, err := s.db.DeletePackageWithBlobs(r.Context(), repoObj.ID)
	if err != nil {
		s.logger.Error("evicting mirror repository", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, d := range purged {
		if err := s.blobs.Delete(r.Context(), d); err != nil {
			s.logger.Warn("evict: failed to delete blob bytes", "digest", d, "error", err)
		}
	}
	writeJSON(w, map[string]any{"evicted": repo, "blobsPurged": len(purged)})
}

func (s *Server) adminGCStatus(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /gc")
	writeJSON(w, s.gc.Status())
}

func (s *Server) adminRunGC(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: POST /gc")
	s.gc.RunOnce(r.Context())
	writeJSON(w, map[string]string{"status": "ok"})
}

// classifyError maps service errors to HTTP status codes.
// Errors from resolving or fetching remote actors are gateway errors;
// everything else is an internal error.
func classifyError(err error) int {
	msg := err.Error()
	for _, prefix := range []string{"resolving target:", "fetching actor:", "delivering follow:"} {
		if strings.HasPrefix(msg, prefix) {
			return http.StatusBadGateway
		}
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON: failed to encode response", "error", err)
	}
}
