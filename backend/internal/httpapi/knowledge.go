package httpapi

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/knowledge"
)

// Knowledge base (P5 remaining modules, batch B). Any authenticated role may
// read documents; create/update/delete are admin-only and audited in the same
// transaction. The stored body is plain text or Markdown: the server validates,
// stores and returns it verbatim and never renders it, so the client must render
// it safely (no raw HTML, allow-listed link protocols).

// knowledgeBodyLimit is the transport bound for a document write: the stored
// body is at most 100 KiB, but JSON escaping can inflate it, so the reader
// accepts up to 256 KiB and the service enforces the business bound.
const knowledgeBodyLimit = 256 << 10

// knowledgeError maps knowledge-base errors to the shared envelope. The
// validation message describes the document contract so a client learns what to
// fix without seeing server internals.
func (s *Server) knowledgeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, knowledge.ErrValidation):
		s.fail(c, 400, "validation_failed", "title 1-200 bytes on one line, body at most 102400 bytes of valid UTF-8, tags at most 200 bytes on one line, and q at most 200 bytes")
	case errors.Is(err, knowledge.ErrForbidden):
		s.fail(c, 403, "forbidden", "knowledge documents are writable by admins only")
	case errors.Is(err, knowledge.ErrNotFound):
		s.fail(c, 404, "not_found", "resource not found")
	default:
		s.logger.ErrorContext(c.Request.Context(), "knowledge request failed", "requestId", requestID(c), "error", err)
		s.fail(c, 500, "internal_error", "internal server error")
	}
}

// listKnowledge pages documents by last update, newest first, with an optional
// title substring search. Every authenticated role may read; list items never
// carry the body.
func (s *Server) listKnowledge(c *gin.Context) {
	if s.knowledge == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	limit, offset, ok := s.page(c)
	if !ok {
		return
	}
	list, total, err := s.knowledge.List(c.Request.Context(), knowledge.ListFilter{
		Query: c.Query("q"), Limit: limit, Offset: offset,
	})
	if err != nil {
		s.knowledgeError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"items": list, "total": total, "limit": limit, "offset": offset})
}

// createKnowledge stores one admin-authored document (201) and audits
// knowledge.create in the same transaction.
func (s *Server) createKnowledge(c *gin.Context) {
	if s.knowledge == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Tags  string `json:"tags"`
	}
	if !s.decodeWithLimit(c, &input, knowledgeBodyLimit, "256 KiB") {
		return
	}
	doc, err := s.knowledge.Create(c.Request.Context(), actorFrom(c),
		knowledge.Input{Title: input.Title, Body: input.Body, Tags: input.Tags})
	if err != nil {
		s.knowledgeError(c, err)
		return
	}
	s.respond(c, 201, doc)
}

// getKnowledge returns one document with its body; an unknown id is a 404.
func (s *Server) getKnowledge(c *gin.Context) {
	if s.knowledge == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	doc, err := s.knowledge.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.knowledgeError(c, err)
		return
	}
	s.respond(c, 200, doc)
}

// updateKnowledge replaces one document's title/body/tags (admin-only) and
// audits knowledge.update; the audit detail never contains the body.
func (s *Server) updateKnowledge(c *gin.Context) {
	if s.knowledge == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Tags  string `json:"tags"`
	}
	if !s.decodeWithLimit(c, &input, knowledgeBodyLimit, "256 KiB") {
		return
	}
	doc, err := s.knowledge.Update(c.Request.Context(), actorFrom(c), c.Param("id"),
		knowledge.Input{Title: input.Title, Body: input.Body, Tags: input.Tags})
	if err != nil {
		s.knowledgeError(c, err)
		return
	}
	s.respond(c, 200, doc)
}

// deleteKnowledge removes one document (admin-only) and audits
// knowledge.delete. An unknown id is a 404.
func (s *Server) deleteKnowledge(c *gin.Context) {
	if s.knowledge == nil {
		s.fail(c, 503, "dependencies_unavailable", "dependencies unavailable")
		return
	}
	if err := s.knowledge.Delete(c.Request.Context(), actorFrom(c), c.Param("id")); err != nil {
		s.knowledgeError(c, err)
		return
	}
	s.respond(c, 200, gin.H{"deleted": true})
}
