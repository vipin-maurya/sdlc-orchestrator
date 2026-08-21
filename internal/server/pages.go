package server

// Read-only pages (spec §5.1). Owner: W2-H.
//
// Created empty by the skeleton commit so server.go's route table can name
// every handler at once; W2-H fills the bodies and owns this file from then on.

import "net/http"

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request)       { stub(w, "W2-H") }
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request)        { stub(w, "W2-H") }
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request)     { stub(w, "W2-H") }
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request)       { stub(w, "W2-H") }
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request)        { stub(w, "W2-H") }
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request)  { stub(w, "W2-H") }
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request)   { stub(w, "W2-H") }
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request)     { stub(w, "W2-H") }
func (s *Server) handleSubmitForm(w http.ResponseWriter, r *http.Request) { stub(w, "W2-H") }
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request)     { stub(w, "W2-H") }
