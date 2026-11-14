package media

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// BuildReviewBundle packages the already-aligned audio together with the
// picture belonging to the take being conformed. The video bytes are the
// immutable picture admitted for that take, so a bundle can never pick up
// another take's frames even when several takes share an audio recording.
func BuildReviewBundle(
	audio []byte,
	video []byte,
	receipt domain.ConformReceipt,
) ([]byte, []byte, error) {
	conformedVideo, _, err := NormalizeY4M(video)
	if err != nil {
		return nil, nil, fmt.Errorf("normalize bundle picture: %w", err)
	}
	receiptData, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode receipt: %w", err)
	}
	receiptData = append(receiptData, '\n')

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	entries := []struct {
		name string
		data []byte
	}{
		{name: "audio-aligned.wav", data: audio},
		{name: "video-conformed.y4m", data: conformedVideo},
		{name: "conform-receipt.json", data: receiptData},
	}
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:     entry.name,
			Method:   zip.Store,
			Modified: time.Unix(0, 0).UTC(),
		}
		header.SetMode(0o600)
		part, err := writer.CreateHeader(header)
		if err != nil {
			return nil, nil, fmt.Errorf("create ZIP entry: %w", err)
		}
		if _, err := part.Write(entry.data); err != nil {
			return nil, nil, fmt.Errorf("write ZIP entry: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("close ZIP bundle: %w", err)
	}
	return output.Bytes(), receiptData, nil
}
