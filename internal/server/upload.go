package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/hanleo-6/mailcat/internal/convert"
	"github.com/hanleo-6/mailcat/internal/store"
)

type savedFile struct {
	Name       string `json:"name"`
	Converting bool   `json:"converting"`
}

type uploadResponse struct {
	Saved []savedFile `json:"saved"`
	Error string      `json:"error,omitempty"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		WriteError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	saved := make([]savedFile, 0)
	var totalBytes int64

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				s.writeUploadFailure(w, http.StatusRequestEntityTooLarge, saved, "upload exceeds max_upload_bytes")
				return
			}
			s.writeUploadFailure(w, http.StatusInternalServerError, saved, "failed reading multipart stream")
			return
		}

		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}

		contentType := part.Header.Get("Content-Type")
		sanitised := store.Sanitise(part.FileName(), contentType)
		finalName, written, pubErr := store.Publish(s.cfg.InboxDir, sanitised, part)
		_ = part.Close()

		if pubErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(pubErr, &maxBytesErr) {
				s.writeUploadFailure(w, http.StatusRequestEntityTooLarge, saved, "upload exceeds max_upload_bytes")
				return
			}
			s.logger.Printf("upload failed for %q after %d bytes: %v", sanitised, written, pubErr)
			s.writeUploadFailure(w, http.StatusInternalServerError, saved, pubErr.Error())
			return
		}

		converting := false
		if s.converter != nil && s.cfg.HEICFormat != "keep" && convert.IsHEIC(finalName, contentType) {
			converting = s.converter.Enqueue(finalName)
		}

		totalBytes += written
		saved = append(saved, savedFile{
			Name:       finalName,
			Converting: converting,
		})
	}

	if len(saved) == 0 {
		WriteError(w, http.StatusBadRequest, "no file parts in upload")
		return
	}

	s.logger.Printf("received %d file(s) (%d bytes)", len(saved), totalBytes)
	s.notifyUpload(saved)
	WriteJSON(w, http.StatusOK, uploadResponse{Saved: saved})
}

func (s *Server) notifyUpload(saved []savedFile) {
	if s.notifier == nil || len(saved) == 0 {
		return
	}
	var ready []savedFile
	for _, f := range saved {
		if !f.Converting {
			ready = append(ready, f)
		}
	}
	if len(ready) == 0 {
		return
	}
	if len(ready) == 1 {
		path := filepath.Join(s.cfg.InboxDir, ready[0].Name)
		s.notifier.Notify("mailcat", "Received "+ready[0].Name, path)
		return
	}
	s.notifier.Notify("mailcat", fmt.Sprintf("Received %d files", len(ready)), s.cfg.InboxDir)
}

func (s *Server) writeUploadFailure(w http.ResponseWriter, status int, saved []savedFile, msg string) {
	if len(saved) > 0 {
		WriteJSON(w, status, uploadResponse{
			Saved: saved,
			Error: msg,
		})
		return
	}
	WriteError(w, status, msg)
}
