package server

// Decision and submit POSTs (spec §5.2). Owner: W2-I.
//
// Created empty by the skeleton commit. Every handler here is already behind
// requireCSRF, and the four decision handlers behind requireFreshState as well
// — that is server.go's job and not this file's, so an owner filling these in
// cannot accidentally register one without a guard.

import (
	"net/http"

	"github.com/vipinm/sdlc-orchestrator/internal/store"
)

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request, j *store.Job) { stub(w, "W2-I") }
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request, j *store.Job)  { stub(w, "W2-I") }
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request, j *store.Job)  { stub(w, "W2-I") }
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, j *store.Job)  { stub(w, "W2-I") }

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request)       { stub(w, "W2-I") }
func (s *Server) handleDiffViewPref(w http.ResponseWriter, r *http.Request) { stub(w, "W2-I") }
