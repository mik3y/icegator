// Package streamlover connects Icegator to a StreamLover station. It reads
// the station's DJs, shows and schedule, checks DJ stream keys, and reports
// what goes to air.
//
// StreamLover has nothing built for Icegator. This package uses the API its
// own dashboard uses, with a station API key in place of a login. The
// contract is described in StreamLover's docs/api-keys.md and
// docs/stream-keys.md.
package streamlover

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client calls one station's API.
type Client struct {
	base    string // e.g. https://streamlover.example.com
	station string // id or slug
	key     string
	http    *http.Client
	// upload has no overall time limit, since a recording can take a
	// while to send. The request's context still bounds it.
	upload *http.Client
}

// NewClient makes a client for the station at baseURL.
func NewClient(baseURL, station, apiKey string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"), station: station, key: apiKey,
		http:   &http.Client{Timeout: 20 * time.Second},
		upload: &http.Client{},
	}
}

// StreamKeyDigest is what StreamLover serves in place of a DJ's stream key,
// computed here over a key a DJ has presented. normalizedKey must already
// be uppercase with no hyphens or spaces.
func (c *Client) StreamKeyDigest(normalizedKey string) string {
	mac := hmac.New(sha256.New, []byte(c.key))
	mac.Write([]byte(normalizedKey))
	return hex.EncodeToString(mac.Sum(nil))
}

// APIError is a reply that says the request was understood and refused.
// Sending the same request again will get the same answer.
type APIError struct {
	Status int
	Body   string
	// Fields is the decoded reply, when it was a JSON object.
	Fields map[string]json.RawMessage
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300]
	}
	return fmt.Sprintf("streamlover: %d: %s", e.Status, body)
}

// Permanent reports whether err is one that retrying cannot fix.
func Permanent(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != 429
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base + "/api/v1/station/" + url.PathEscape(c.station) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Body: string(raw)}
		json.Unmarshal(raw, &apiErr.Fields)
		return apiErr
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Show is a programme on the station.
type Show struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Hosts []Host `json:"hosts"`
}

// Host is a DJ profile.
type Host struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// StreamKeyHMAC is empty for a DJ with no stream key.
	StreamKeyHMAC string `json:"stream_key_hmac"`
}

// Occurrence is one scheduled airing of a show.
type Occurrence struct {
	ScheduleID    string    `json:"schedule_id"`
	OriginalStart time.Time `json:"original_start"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Show          struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"show"`
}

func (c *Client) Hosts(ctx context.Context) ([]Host, error) {
	var out []Host
	return out, c.do(ctx, "GET", "/hosts", nil, nil, &out)
}

func (c *Client) Shows(ctx context.Context) ([]Show, error) {
	var out []Show
	return out, c.do(ctx, "GET", "/shows", nil, nil, &out)
}

func (c *Client) Occurrences(ctx context.Context, from, to time.Time) ([]Occurrence, error) {
	q := url.Values{"start": {from.UTC().Format(time.RFC3339)}, "end": {to.UTC().Format(time.RFC3339)}}
	var out []Occurrence
	return out, c.do(ctx, "GET", "/schedule/occurrences", q, nil, &out)
}

// Track is one entry in a broadcast's tracklist.
type Track struct {
	Name          string `json:"name"`
	Artist        string `json:"artist"`
	Album         string `json:"album"`
	OffsetSeconds *int   `json:"offset_seconds"`
}

// Broadcast is the record that a show aired.
type Broadcast struct {
	ID         string     `json:"id"`
	StartDate  time.Time  `json:"start_date"`
	EndDate    *time.Time `json:"end_date"`
	Visibility string     `json:"visibility"`
	Tracks     []Track    `json:"tracks"`
	Recordings []struct {
		ID string `json:"id"`
	} `json:"recordings"`
}

// ErrAiringHasBroadcast is returned by CreateBroadcast when the scheduled
// airing the new broadcast would belong to already has one.
type ErrAiringHasBroadcast struct{ BroadcastID string }

func (e *ErrAiringHasBroadcast) Error() string {
	return "streamlover: this scheduled airing already has broadcast " + e.BroadcastID
}

// CreateBroadcast logs that a show went on air at start.
func (c *Client) CreateBroadcast(ctx context.Context, showID string, start time.Time, visibility string) (*Broadcast, error) {
	body := map[string]any{"show_id": showID, "start_date": start.UTC().Format(time.RFC3339Nano)}
	if visibility != "" {
		body["visibility"] = visibility
	}
	var out Broadcast
	err := c.do(ctx, "POST", "/broadcasts", nil, body, &out)
	var api *APIError
	if errors.As(err, &api) && api.Status == 400 {
		// The id arrives as a one-element list, the way the framework
		// reports any field of a refused request.
		var ids []string
		if json.Unmarshal(api.Fields["broadcast_id"], &ids) == nil && len(ids) == 1 {
			return nil, &ErrAiringHasBroadcast{BroadcastID: ids[0]}
		}
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Broadcast(ctx context.Context, id string) (*Broadcast, error) {
	var out Broadcast
	return &out, c.do(ctx, "GET", "/broadcasts/"+url.PathEscape(id), nil, nil, &out)
}

// UpdateBroadcast changes the given fields of a broadcast.
func (c *Client) UpdateBroadcast(ctx context.Context, id string, fields map[string]any) error {
	return c.do(ctx, "PATCH", "/broadcasts/"+url.PathEscape(id), nil, fields, nil)
}

// SetTracks replaces a broadcast's whole tracklist.
func (c *Client) SetTracks(ctx context.Context, id string, tracks []Track) error {
	if tracks == nil {
		tracks = []Track{}
	}
	return c.do(ctx, "PUT", "/broadcasts/"+url.PathEscape(id)+"/tracks", nil, map[string]any{"tracks": tracks}, nil)
}

// MaxUploadBytes is the largest file StreamLover accepts as an upload.
const MaxUploadBytes = 500 * 1024 * 1024

// ErrTooLarge is returned by UploadAudio for a file over MaxUploadBytes.
var ErrTooLarge = errors.New("streamlover: the file is larger than the 500 MB it accepts")

// UploadAudio uploads an MP3 file to StreamLover's storage and returns the
// id it is known by there. seconds and kbps describe the audio, which
// StreamLover does not work out for itself.
func (c *Client) UploadAudio(ctx context.Context, path string, seconds float64, kbps int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if stat.Size() > MaxUploadBytes {
		return "", ErrTooLarge
	}

	// StreamLover hands out a one-time address, and the file goes straight
	// from here to its storage.
	var slot struct {
		AudioFile struct {
			ID string `json:"id"`
		} `json:"audio_file"`
		UploadURL string            `json:"upload_url"`
		Headers   map[string]string `json:"headers"`
	}
	ask := map[string]any{"filename": filepath.Base(path), "mime_type": "audio/mpeg", "size_bytes": stat.Size()}
	if err := c.do(ctx, "POST", "/audio", nil, ask, &slot); err != nil {
		return "", err
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return "", err
	}
	target, err := base.Parse(slot.UploadURL)
	if err != nil {
		return "", fmt.Errorf("streamlover: bad upload address %q: %w", slot.UploadURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", target.String(), f)
	if err != nil {
		return "", err
	}
	req.ContentLength = stat.Size()
	for name, value := range slot.Headers {
		req.Header.Set(name, value)
	}
	// In production the address is a presigned one on the storage service,
	// which wants no credentials and would refuse a request that carried
	// some. A development StreamLover takes the upload itself, and asks for
	// the key again.
	if target.Host == base.Host {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.upload.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &APIError{Status: resp.StatusCode, Body: "upload: " + string(body)}
	}

	done := map[string]any{"duration_seconds": seconds, "bitrate": kbps}
	if err := c.do(ctx, "POST", "/audio/"+url.PathEscape(slot.AudioFile.ID)+"/complete", nil, done, nil); err != nil {
		return "", err
	}
	return slot.AudioFile.ID, nil
}

// AttachAudio makes an uploaded file the recording of a broadcast.
func (c *Client) AttachAudio(ctx context.Context, broadcastID, audioFileID string) error {
	body := map[string]any{"audio_file_id": audioFileID}
	err := c.do(ctx, "POST", "/broadcasts/"+url.PathEscape(broadcastID)+"/recordings", nil, body, nil)
	var api *APIError
	if errors.As(err, &api) && api.Status == 400 && strings.Contains(api.Body, "already attached") {
		// An earlier attempt got through and its reply was lost.
		return nil
	}
	return err
}
