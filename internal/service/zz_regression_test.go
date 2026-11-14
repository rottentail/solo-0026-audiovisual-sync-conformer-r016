package service_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
	appservice "github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/service"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/storage"
)

// buildVideo renders a C420 Y4M clip whose single luma flash sits at flashFrame.
func buildVideo(t *testing.T, flashFrame int) []byte {
	t.Helper()
	width, height := 96, 54
	frameBytes := width*height + 2*(width/2)*(height/2)
	frames := make([][]byte, 0, media.CalibrationFrames)
	for i := 0; i < media.CalibrationFrames; i++ {
		frame := make([]byte, frameBytes)
		luma := byte(32)
		if i == flashFrame {
			luma = 220
		}
		for p := 0; p < width*height; p++ {
			frame[p] = luma
		}
		frames = append(frames, frame)
	}
	info := domain.VideoInfo{
		Width:      width,
		Height:     height,
		FPSNum:     media.CalibrationFPS,
		FPSDen:     1,
		FrameCount: len(frames),
		Chroma:     "C420jpeg",
	}
	video, err := media.EncodeY4M(info, frames)
	if err != nil {
		t.Fatalf("encode video: %v", err)
	}
	return video
}

func zipEntry(t *testing.T, data []byte, name string) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	for _, entry := range reader.File {
		if entry.Name == name {
			rc, err := entry.Open()
			if err != nil {
				t.Fatalf("open entry: %v", err)
			}
			defer rc.Close()
			content, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read entry: %v", err)
			}
			return content
		}
	}
	t.Fatalf("entry %s missing", name)
	return nil
}

func admit(t *testing.T, svc *appservice.Service, id, name string, audio, video []byte) domain.MediaTake {
	t.Helper()
	result, err := svc.CreateTake(domain.CreateTakeInput{
		TakeID:      id,
		Name:        name,
		AudioBase64: stdBase64(audio),
		VideoBase64: stdBase64(video),
	})
	if err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
	return result.Take
}

// TestSharedAudioTakesKeepOwnAudiovisual reproduces the field incident: two
// camera positions share one recording but carry different pictures. Each take
// must retain its own original audio/picture through analysis, conform, a
// simulated service restart, and concurrent or failed admissions of the other.
func TestSharedAudioTakesKeepOwnAudiovisual(t *testing.T) {
	root := t.TempDir()
	repo, err := storage.New(root)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	svc := appservice.New(repo)

	audio, sharedVideo, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration pair: %v", err)
	}
	// Two distinct pictures with different flash positions, identical geometry.
	videoA := sharedVideo
	videoB := buildVideo(t, 17)
	if media.DigestBytes(videoA) == media.DigestBytes(videoB) {
		t.Fatal("test fixtures must differ in picture content")
	}

	takeA := admit(t, svc, "cam-take-a", "camera position A", audio, videoA)
	takeB := admit(t, svc, "cam-take-b", "camera position B", audio, videoB)

	// 1. Stored media for each take is its own immutable audio/picture pair.
	gotAudioA, gotVideoA, err := repo.ReadTakeMedia("cam-take-a")
	if err != nil {
		t.Fatalf("read media A: %v", err)
	}
	gotAudioB, gotVideoB, err := repo.ReadTakeMedia("cam-take-b")
	if err != nil {
		t.Fatalf("read media B: %v", err)
	}
	if media.DigestBytes(gotAudioA) != media.DigestBytes(audio) ||
		media.DigestBytes(gotAudioB) != media.DigestBytes(audio) {
		t.Fatal("shared audio was not preserved per take")
	}
	if media.DigestBytes(gotVideoA) != media.DigestBytes(videoA) {
		t.Fatal("take A picture was overwritten by the group indirection")
	}
	if media.DigestBytes(gotVideoB) != media.DigestBytes(videoB) {
		t.Fatal("take B picture does not match its admitted bytes")
	}
	if takeA.Digest == takeB.Digest {
		t.Fatal("takes with different picture bytes must have different combined digests")
	}

	// 2. Repeated analyses of A always yield A's flash position, never B's.
	analysisA, err := svc.Analyze("cam-take-a", domain.CreateAnalysisInput{
		AnalysisID: "sync-a", MinConfidence: 0.55, MaxOffsetMS: 1000,
	})
	if err != nil {
		t.Fatalf("analyze A: %v", err)
	}
	analysisB, err := svc.Analyze("cam-take-b", domain.CreateAnalysisInput{
		AnalysisID: "sync-b", MinConfidence: 0.55, MaxOffsetMS: 1000,
	})
	if err != nil {
		t.Fatalf("analyze B: %v", err)
	}
	if analysisA.Analysis.VideoEventMS == nil || analysisB.Analysis.VideoEventMS == nil {
		t.Fatal("analyses did not produce picture events")
	}
	flashA := 9 * 1000 / media.CalibrationFPS
	flashB := 17 * 1000 / media.CalibrationFPS
	if *analysisA.Analysis.VideoEventMS != flashA {
		t.Fatalf("take A flash = %d, want %d (leaked take B picture?)", *analysisA.Analysis.VideoEventMS, flashA)
	}
	if *analysisB.Analysis.VideoEventMS != flashB {
		t.Fatalf("take B flash = %d, want %d", *analysisB.Analysis.VideoEventMS, flashB)
	}

	// 3. Conform A and capture its delivered bundle bytes.
	conformA, err := svc.CreateConform("sync-a", domain.CreateConformInput{
		ConformID: "review-a", MaxCorrectionMS: 500,
	})
	if err != nil {
		t.Fatalf("conform A: %v", err)
	}
	if conformA.Conform.State != domain.ConformStateCompleted {
		t.Fatalf("conform A state = %s", conformA.Conform.State)
	}
	bundleABefore, receiptBefore, err := svc.ReadConformBundle("review-a")
	if err != nil {
		t.Fatalf("read bundle A: %v", err)
	}
	if receiptBefore.InputTakeDigest != takeA.Digest {
		t.Fatalf("receipt input digest = %q, want take digest %q", receiptBefore.InputTakeDigest, takeA.Digest)
	}
	pictureInBundleA := zipEntry(t, bundleABefore, "video-conformed.y4m")
	normalizedA, _, err := media.NormalizeY4M(videoA)
	if err != nil {
		t.Fatalf("normalize take A picture: %v", err)
	}
	if !bytes.Equal(pictureInBundleA, normalizedA) {
		t.Fatal("delivered bundle A does not contain take A's picture")
	}
	conformBDigest := ""
	{
		conformB, err := svc.CreateConform("sync-b", domain.CreateConformInput{
			ConformID: "review-b", MaxCorrectionMS: 500,
		})
		if err != nil {
			t.Fatalf("conform B: %v", err)
		}
		conformBDigest = conformB.Conform.BundleDigest
		bundleB, _, err := svc.ReadConformBundle("review-b")
		if err != nil {
			t.Fatalf("read bundle B: %v", err)
		}
		if media.DigestBytes(bundleB) == media.DigestBytes(bundleABefore) {
			t.Fatal("bundles for different pictures must not be identical")
		}
		// B's bundle must carry B's flash.
		bundleBPicture := zipEntry(t, bundleB, "video-conformed.y4m")
		measurement, err := media.AnalyzeSync(zipEntry(t, bundleB, "audio-aligned.wav"), bundleBPicture)
		if err != nil {
			t.Fatalf("analyze bundle B: %v", err)
		}
		if measurement.VideoEventMS != flashB {
			t.Fatalf("bundle B flash = %d, want %d", measurement.VideoEventMS, flashB)
		}
	}

	// 4. Concurrent admissions of additional shared-audio takes must not disturb
	// existing payloads or the already delivered bundle.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = svc.CreateTake(domain.CreateTakeInput{
				TakeID:      "cam-parallel-" + string(rune('a'+i)),
				Name:        "parallel camera",
				AudioBase64: stdBase64(audio),
				VideoBase64: stdBase64(buildVideo(t, 3+i)),
			})
		}(i)
	}
	wg.Wait()

	// 5. A failed admission (invalid media) leaves no visible take and no shared
	// state that breaks the established takes.
	if _, err := svc.CreateTake(domain.CreateTakeInput{
		TakeID:      "cam-broken",
		Name:        "broken camera",
		AudioBase64: stdBase64([]byte("not a wave payload")),
		VideoBase64: stdBase64(videoA),
	}); err == nil {
		t.Fatal("invalid audio must be rejected")
	}
	if _, err := repo.GetTake("cam-broken"); err == nil {
		t.Fatal("failed admission left a visible take")
	}

	// 6. Simulate a service restart: a brand new repository/service over the same
	// data root, with an empty in-process cache. Ownership must survive.
	restartedRepo, err := storage.New(root)
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	restartedSvc := appservice.New(restartedRepo)
	restartedVideoA, err := func() ([]byte, error) {
		_, v, e := restartedRepo.ReadTakeMedia("cam-take-a")
		return v, e
	}()
	if err != nil {
		t.Fatalf("restart read A: %v", err)
	}
	if media.DigestBytes(restartedVideoA) != media.DigestBytes(videoA) {
		t.Fatal("take A picture changed after restart")
	}
	reAnalyzed, err := restartedSvc.Analyze("cam-take-a", domain.CreateAnalysisInput{
		AnalysisID: "sync-a-again", MinConfidence: 0.55, MaxOffsetMS: 1000,
	})
	if err != nil {
		t.Fatalf("re-analyze A after restart: %v", err)
	}
	if *reAnalyzed.Analysis.VideoEventMS != flashA {
		t.Fatalf("post-restart take A flash = %d, want %d", *reAnalyzed.Analysis.VideoEventMS, flashA)
	}
	bundleAAfter, receiptAfter, err := restartedSvc.ReadConformBundle("review-a")
	if err != nil {
		t.Fatalf("read bundle A after restart: %v", err)
	}
	if media.DigestBytes(bundleAAfter) != media.DigestBytes(bundleABefore) {
		t.Fatal("delivered bundle A changed after another take was admitted and the service restarted")
	}
	if receiptAfter.InputTakeDigest != takeA.Digest {
		t.Fatal("receipt digest changed after restart")
	}
	// Bundle A still shows A's flash and never resolves to conform B.
	afterPicture := zipEntry(t, bundleAAfter, "video-conformed.y4m")
	afterMeasurement, err := media.AnalyzeSync(zipEntry(t, bundleAAfter, "audio-aligned.wav"), afterPicture)
	if err != nil {
		t.Fatalf("analyze bundle A after restart: %v", err)
	}
	if afterMeasurement.VideoEventMS != flashA {
		t.Fatalf("post-restart bundle A flash = %d, want %d (redirected to take B?)", afterMeasurement.VideoEventMS, flashA)
	}
	jobB, err := restartedSvc.GetConform("review-b")
	if err != nil {
		t.Fatalf("read conform B: %v", err)
	}
	if jobB.BundleDigest != conformBDigest {
		t.Fatal("conform B record changed after restart")
	}

	// 7. No audio-groups namespace may exist on disk: ownership lives in the
	// exclusive take namespace only.
	if entries, err := filepath.Glob(filepath.Join(root, "audio-groups", "*")); err == nil && len(entries) != 0 {
		t.Fatalf("legacy shared group state still present: %v", entries)
	}
}

func stdBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
