// Package sniff 按 magic bytes 嗅探真实媒体类型 —— 绝不信任扩展名或 Content-Type。
//
// 真实案例（原项目生产实测）：一个以 `.jpg` 结尾、Content-Type 写 `image/jpg`
// （不是合法 MIME）的链接，返回的字节其实是 PNG。站点按 Content-Type 校验，
// 直接回 `Unsupported upload content type`。
//
// 上传上限按类型分档（站点预签名响应里的 maxBytes）：
//
//	图片 10MB / 视频 50MB / 音频 15MB
package sniff

import "encoding/binary"

// Info 是一次嗅探的结论。
type Info struct {
	ContentType string `json:"content_type"`
	Ext         string `json:"ext"`
	Kind        string `json:"kind"` // image | video | audio | unknown
}

// MaxUploadBytes 是站点分类型上限里的**最大者**（视频 50MB）。
// 单件媒体的硬上限在任何入口都按它挡 —— 早点拒比"传上去再被 maxBytes 拒"好。
const MaxUploadBytes = 50 * 1024 * 1024

// File 嗅探字节流。
func File(buf []byte) Info {
	n := len(buf)
	switch {
	case n >= 8 && string(buf[:4]) == "\x89PNG":
		return Info{"image/png", "png", "image"}
	case n >= 3 && buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF:
		return Info{"image/jpeg", "jpg", "image"}
	case n >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP":
		return Info{"image/webp", "webp", "image"}
	case n >= 12 && string(buf[4:8]) == "ftyp":
		// ISO base media（MP4 / MOV / M4A）：'ftyp' 在 offset 4，brand 在 offset 8
		brand := string(buf[8:12])
		if brand == "qt  " {
			return Info{"video/quicktime", "mov", "video"}
		}
		if brand[:3] == "M4A" || brand[:3] == "M4B" {
			return Info{"audio/mp4", "m4a", "audio"}
		}
		return Info{"video/mp4", "mp4", "video"}
	case n >= 4 && string(buf[:4]) == "\x1a\x45\xdf\xa3":
		return Info{"video/webm", "webm", "video"}
	case n >= 3 && string(buf[:3]) == "ID3":
		return Info{"audio/mpeg", "mp3", "audio"}
	case n >= 2 && buf[0] == 0xFF && (buf[1]&0xE0) == 0xE0:
		// ADTS AAC（0xFFF1 / 0xFFF9）与 MPEG 音频帧同步（0xFFFB / 0xFFF3 …）要分开
		if (buf[1] & 0xF6) == 0xF0 {
			return Info{"audio/aac", "aac", "audio"}
		}
		return Info{"audio/mpeg", "mp3", "audio"}
	case n >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WAVE":
		return Info{"audio/wav", "wav", "audio"}
	case n >= 4 && string(buf[:4]) == "OggS":
		return Info{"audio/ogg", "ogg", "audio"}
	}
	return Info{"application/octet-stream", "bin", "unknown"}
}

// ImageDimensions 读 PNG / JPEG 的像素尺寸，读不出来返回 (0, 0)。零依赖。
func ImageDimensions(buf []byte) (int, int) {
	// PNG：IHDR 的宽高在 offset 16 / 20
	if len(buf) > 24 && string(buf[1:4]) == "PNG" {
		w := binary.BigEndian.Uint32(buf[16:20])
		h := binary.BigEndian.Uint32(buf[20:24])
		return int(w), int(h)
	}
	// JPEG：扫描 SOFn 段
	if len(buf) > 3 && buf[0] == 0xFF && buf[1] == 0xD8 {
		i := 2
		for i < len(buf)-9 {
			if buf[i] != 0xFF {
				i++
				continue
			}
			marker := buf[i+1]
			if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
				h := int(binary.BigEndian.Uint16(buf[i+5 : i+7]))
				w := int(binary.BigEndian.Uint16(buf[i+7 : i+9]))
				return w, h
			}
			segLen := int(binary.BigEndian.Uint16(buf[i+2 : i+4]))
			if segLen < 2 {
				i++
				continue
			}
			i += 2 + segLen
		}
	}
	return 0, 0
}
