package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSaveAttachments_Empty(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	if got := svc.saveAttachments(context.Background(), "sess", nil); got != "" {
		t.Fatalf("no attachments should yield no block, got %q", got)
	}
}

func TestSaveAttachments_DownloadsAndBuildsBlock(t *testing.T) {
	svc, _, _, _, fs := newTestService()
	svc.fetchURL = func(_ context.Context, url string) ([]byte, error) {
		return []byte("body:" + url), nil
	}

	block := svc.saveAttachments(context.Background(), "sess", []Attachment{
		{Filename: "image.png", URL: "http://x/1", ContentType: "image/png"},
		{Filename: "image.png", URL: "http://x/2", ContentType: "image/png"}, // duplicate name
	})

	if got, ok := fs.files["/state/sessions/sess/attachments/image.png"]; !ok || string(got) != "body:http://x/1" {
		t.Fatalf("first file = %q ok=%v; files=%v", got, ok, fs.files)
	}
	if _, ok := fs.files["/state/sessions/sess/attachments/image-2.png"]; !ok {
		t.Fatalf("duplicate filename should be de-duplicated; files=%v", fs.files)
	}
	if !strings.Contains(block, "<quack-attachments>") ||
		!strings.Contains(block, "/state/sessions/sess/attachments/image.png (image/png)") {
		t.Fatalf("block does not reference the saved file by path: %q", block)
	}
}

func TestSaveAttachments_DownloadFailureNoted(t *testing.T) {
	svc, _, _, _, _ := newTestService()
	svc.fetchURL = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	block := svc.saveAttachments(context.Background(), "sess", []Attachment{
		{Filename: "a.png", URL: "http://x/1"},
	})
	if !strings.Contains(block, "a.png") || !strings.Contains(block, "failed") {
		t.Fatalf("a failed download should be noted in the block, got %q", block)
	}
}
