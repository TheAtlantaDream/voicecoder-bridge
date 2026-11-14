package whisper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type Transcriber interface {
	Transcribe(ctx context.Context, pcm []byte) (string, error)
}

type HTTPClient struct {
	baseURL string
	client  *http.Client
}

func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

func (c *HTTPClient) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}

	wav := wavBytes(pcm)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	_ = w.WriteField("response_format", "json")
	if err := w.Close(); err != nil {
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/inference", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper server at %s: %w (is whisper-server running?)", c.baseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper server: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseTranscript(raw)
}

func parseTranscript(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if trimmed[0] == '[' {
		var segs []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &segs); err != nil {
			return "", fmt.Errorf("parse whisper segments: %w (body: %.200s)", err, trimmed)
		}
		var parts []string
		for _, s := range segs {
			if t := strings.TrimSpace(s.Text); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " "), nil
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return strings.TrimSpace(string(trimmed)), nil
	}
	return strings.TrimSpace(obj.Text), nil
}

func wavBytes(pcm []byte) []byte {
	const (
		sampleRate    = 16000
		bitsPerSample = 16
		channels      = 1
	)

	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)

	var buf bytes.Buffer
	_, _ = buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	_, _ = buf.WriteString("WAVE")
	_, _ = buf.WriteString("fmt ")

	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // subchunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	_, _ = buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	_, _ = buf.Write(pcm)
	return buf.Bytes()
}
