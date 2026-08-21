package server

// The diff viewer (spec §8). Owner: W2-K.
//
// Created empty by the skeleton commit. The parsed-diff cache these handlers
// read through is server.go's diffFor; this file is its only user.

import "net/http"

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request)      { stub(w, "W2-K") }
func (s *Server) handleDiffFile(w http.ResponseWriter, r *http.Request)  { stub(w, "W2-K") }
func (s *Server) handleDiffPatch(w http.ResponseWriter, r *http.Request) { stub(w, "W2-K") }
