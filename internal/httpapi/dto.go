package httpapi

import "github.com/bjoernaagaard/aakb"

type capabilitiesResponse struct {
	Version      string `json:"version"`
	OpenAlex     bool   `json:"openalex"`
	AuthRequired bool   `json:"auth_required"`
}

type readInput struct {
	ID             string `json:"id"`
	Continuation   string `json:"continuation"`
	Limit          int    `json:"limit"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type downloadInput struct {
	Hash           string `json:"hash"`
	DOI            string `json:"doi"`
	OpenAlexID     string `json:"openalex_id"`
	Title          string `json:"title"`
	Format         string `json:"format"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type downloadResponse struct {
	Name          string            `json:"name"`
	FileURL       string            `json:"file_url"`
	Bytes         int64             `json:"bytes"`
	Verification  aakb.Verification `json:"verification"`
	MD5           string            `json:"md5,omitempty"`
	SHA256        string            `json:"sha256,omitempty"`
	Reused        bool              `json:"reused"`
	DownloadsLeft *int              `json:"downloads_left,omitzero"`
}
