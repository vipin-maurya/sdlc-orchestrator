package server

// The review document as HTML (spec §7). Owner: W2-J.
//
// Created empty by the skeleton commit. gateDoc's contract, including the
// rationale comment W2-J must reproduce, is pinned in the plan's §2.7.

import (
	"context"
	"errors"
	"net/http"

	"github.com/vipinm/sdlc-orchestrator/internal/review"
	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

type docSource string

const (
	docLive      docSource = "live"
	docPersisted docSource = "persisted"
)

func (s *Server) handleGate(w http.ResponseWriter, r *http.Request) { stub(w, "W2-J") }

func (s *Server) gateDoc(ctx context.Context, j *store.Job, gate string) (*review.Doc, docSource, error) {
	return nil, docLive, errors.New("gateDoc: not implemented yet — owner W2-J has not landed this")
}
