package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// OutlineClient talks to a self-hosted Outline instance.
type OutlineClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewOutlineClient builds a client for the given base URL (e.g. https://outline.example.com)
// and API token (Bearer credential created in Outline > Settings > API Tokens).
func NewOutlineClient(baseURL, token string) *OutlineClient {
	return &OutlineClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

type createDocumentRequest struct {
	Title        string `json:"title"`
	Text         string `json:"text"`
	CollectionID string `json:"collectionId"`
	Publish      bool   `json:"publish"`
}

type createDocumentResponse struct {
	Data struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"data"`
}

// CreateDocument publishes a new document into the given collection and returns its ID.
func (c *OutlineClient) CreateDocument(collectionID, title, text string) (string, error) {
	body, err := json.Marshal(createDocumentRequest{
		Title:        title,
		Text:         text,
		CollectionID: collectionID,
		Publish:      true,
	})
	if err != nil {
		return "", fmt.Errorf("marshal documents.create: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/documents.create", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build documents.create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("documents.create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("documents.create status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode documents.create response: %w", err)
	}
	return out.Data.ID, nil
}

type createAttachmentRequest struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type createAttachmentResponse struct {
	Data struct {
		UploadURL  string            `json:"uploadUrl"`
		Form       map[string]string `json:"form"`
		Attachment struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"attachment"`
	} `json:"data"`
}

// UploadAttachment uploads a file to Outline and returns a URL safe to embed in markdown.
// Outline's flow is two requests: ask for a presigned upload, then POST the bytes to it.
func (c *OutlineClient) UploadAttachment(name, contentType string, data []byte) (string, error) {
	reqBody, err := json.Marshal(createAttachmentRequest{
		Name:        name,
		ContentType: contentType,
		Size:        int64(len(data)),
	})
	if err != nil {
		return "", fmt.Errorf("marshal attachments.create: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+"/api/attachments.create", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build attachments.create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("attachments.create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("attachments.create status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var presign createAttachmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&presign); err != nil {
		return "", fmt.Errorf("decode attachments.create response: %w", err)
	}

	// Build multipart body: form fields first, file last (S3 requires this order).
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for k, v := range presign.Data.Form {
		if err := writer.WriteField(k, v); err != nil {
			return "", fmt.Errorf("write form field %s: %w", k, err)
		}
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write file bytes: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	uploadURL := presign.Data.UploadURL
	if strings.HasPrefix(uploadURL, "/") {
		uploadURL = c.baseURL + uploadURL
	}

	uploadReq, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	// Outline's local file storage requires the API token; S3 ignores it.
	uploadReq.Header.Set("Authorization", "Bearer "+c.token)

	uploadResp, err := c.httpClient.Do(uploadReq)
	if err != nil {
		return "", fmt.Errorf("upload to %s: %w", uploadURL, err)
	}
	defer uploadResp.Body.Close()

	if uploadResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(uploadResp.Body)
		return "", fmt.Errorf("upload status %d: %s", uploadResp.StatusCode, strings.TrimSpace(string(raw)))
	}

	attachmentURL := presign.Data.Attachment.URL
	if strings.HasPrefix(attachmentURL, "/") {
		attachmentURL = c.baseURL + attachmentURL
	}
	return attachmentURL, nil
}

// DownloadFile fetches the bytes at fileURL.
func DownloadFile(fileURL string) ([]byte, error) {
	resp, err := http.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", fileURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", fileURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
