package media

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

func PackPicture(picture []byte) ([]byte, error) {
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(picture); err != nil {
		writer.Close()
		return nil, fmt.Errorf("compress picture: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish picture compression: %w", err)
	}
	return output.Bytes(), nil
}

func UnpackPicture(packed []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, fmt.Errorf("open compressed picture: %w", err)
	}
	picture, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("decompress picture: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close compressed picture: %w", closeErr)
	}
	return picture, nil
}

type BundlePictureSource interface {
	ReadGroupPicture(group string) ([]byte, error)
}

type pictureReference struct {
	Version int    `json:"version"`
	Group   string `json:"group"`
}

func PackPictureReference(group string) ([]byte, error) {
	if decoded, err := hex.DecodeString(group); err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid picture group")
	}
	return json.Marshal(pictureReference{Version: 1, Group: group})
}

func ReadPictureReference(encoded []byte, source BundlePictureSource) ([]byte, error) {
	var ref pictureReference
	if err := json.Unmarshal(encoded, &ref); err != nil {
		return nil, fmt.Errorf("decode picture reference: %w", err)
	}
	if ref.Version != 1 {
		return nil, fmt.Errorf("unsupported picture reference version")
	}
	if decoded, err := hex.DecodeString(ref.Group); err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid picture group")
	}
	return source.ReadGroupPicture(ref.Group)
}

func BuildReviewBundle(
	audio []byte,
	video []byte,
	receipt domain.ConformReceipt,
	source BundlePictureSource,
) ([]byte, []byte, error) {
	clip, err := InspectVideo(video)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect bundle picture: %w", err)
	}
	selected, err := source.ReadGroupPicture(VideoGroupKey(clip.Info))
	if err != nil {
		return nil, nil, fmt.Errorf("read bundle source picture: %w", err)
	}
	video, _, err = NormalizeY4M(selected)
	if err != nil {
		return nil, nil, fmt.Errorf("normalize bundle source picture: %w", err)
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
		{name: "video-conformed.y4m", data: video},
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
