package sniff

import "testing"

func TestFileByMagicBytes(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want Info
	}{
		{"png", []byte("\x89PNG\r\n\x1a\nrest"), Info{"image/png", "png", "image"}},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, Info{"image/jpeg", "jpg", "image"}},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP"), Info{"image/webp", "webp", "image"}},
		{"mp4", []byte("\x00\x00\x00\x18ftypisom"), Info{"video/mp4", "mp4", "video"}},
		{"mov", []byte("\x00\x00\x00\x14ftypqt  "), Info{"video/quicktime", "mov", "video"}},
		{"m4a", []byte("\x00\x00\x00\x14ftypM4A "), Info{"audio/mp4", "m4a", "audio"}},
		{"webm", []byte("\x1a\x45\xdf\xa3rest"), Info{"video/webm", "webm", "video"}},
		{"mp3-id3", []byte("ID3\x04\x00"), Info{"audio/mpeg", "mp3", "audio"}},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), Info{"audio/wav", "wav", "audio"}},
		{"ogg", []byte("OggS\x00"), Info{"audio/ogg", "ogg", "audio"}},
		{"unknown", []byte("hello world"), Info{"application/octet-stream", "bin", "unknown"}},
		{"empty", nil, Info{"application/octet-stream", "bin", "unknown"}},
	}
	for _, c := range cases {
		if got := File(c.buf); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestImageDimensions(t *testing.T) {
	// 2x3 PNG：IHDR 宽高在 offset 16/20。
	png := make([]byte, 33)
	copy(png, "\x89PNG\r\n\x1a\n")
	png[12], png[13], png[14], png[15] = 'I', 'H', 'D', 'R'
	png[19] = 2 // width  = 2
	png[23] = 3 // height = 3
	if w, h := ImageDimensions(png); w != 2 || h != 3 {
		t.Errorf("png dimensions = %dx%d, want 2x3", w, h)
	}
	if w, h := ImageDimensions([]byte("nope")); w != 0 || h != 0 {
		t.Errorf("unreadable input must return 0x0, got %dx%d", w, h)
	}
}
