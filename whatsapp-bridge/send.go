package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Function to send a WhatsApp message to a chat that passed checkSendAllowed
func (b *Bridge) sendWhatsAppMessage(ctx context.Context, chatJID string, message string, mediaPath string) *apiError {
	if !b.client.IsConnected() {
		return refuse(http.StatusServiceUnavailable, "Not connected to WhatsApp")
	}

	recipientJID, err := types.ParseJID(chatJID)
	if err != nil {
		return refuse(http.StatusBadRequest, "Error parsing JID: %v", err)
	}

	msg := &waE2E.Message{}
	var sentMedia MediaInfo

	// Check if we have media to send
	if mediaPath != "" {
		// The store holds the session keys and the API token; those never leave this machine
		if b.insideStore(mediaPath) {
			return refuse(http.StatusForbidden, "Files inside the bridge's store directory cannot be sent")
		}

		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return refuse(http.StatusBadRequest, "Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileName := filepath.Base(mediaPath)
		fileExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(mediaPath), "."))
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = mime.TypeByExtension("." + fileExt)
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
		}

		// Upload media to WhatsApp servers
		resp, err := b.client.Upload(ctx, mediaData, mediaType)
		if err != nil {
			return refuse(http.StatusInternalServerError, "Error uploading media: %v", err)
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			sentMedia.Type = "image"
			msg.ImageMessage = &waE2E.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			sentMedia.Type = "audio"
			// Handle ogg audio files
			seconds, waveform, err := analyzeOggOpus(mediaData)
			if err != nil {
				return refuse(http.StatusBadRequest, "Failed to analyze Ogg Opus file: %v", err)
			}

			msg.AudioMessage = &waE2E.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			sentMedia.Type = "video"
			msg.VideoMessage = &waE2E.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			sentMedia.Type = "document"
			sentMedia.Filename = fileName
			msg.DocumentMessage = &waE2E.DocumentMessage{
				Title:         proto.String(fileName),
				FileName:      proto.String(fileName),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}

		sentMedia.MimeType = mimeType
		sentMedia.URL = resp.URL
		sentMedia.DirectPath = resp.DirectPath
		sentMedia.MediaKey = resp.MediaKey
		sentMedia.FileSHA256 = resp.FileSHA256
		sentMedia.FileEncSHA256 = resp.FileEncSHA256
		sentMedia.FileLength = resp.FileLength
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	resp, err := b.client.SendMessage(ctx, recipientJID, msg)
	if err != nil {
		return refuse(http.StatusInternalServerError, "Error sending message: %v", err)
	}

	// WhatsApp does not echo a device's own messages back to it, so record the message here
	b.storeLock.Lock()
	defer b.storeLock.Unlock()
	if err := b.store.StoreChat(chatJID, recipientJID.User, resp.Timestamp); err != nil {
		b.logger.Warnf("Failed to store chat: %v", err)
	}
	sender := ""
	if b.client.Store.ID != nil {
		sender = b.client.Store.ID.User
	}
	err = b.store.StoreMessage(StoredMessage{
		ID:        resp.ID,
		ChatJID:   chatJID,
		Sender:    sender,
		Content:   message,
		Timestamp: resp.Timestamp,
		IsFromMe:  true,
		Media:     sentMedia,
	})
	if err != nil {
		b.logger.Warnf("Failed to store sent message: %v", err)
	}

	return nil
}

// resolvePath makes a path absolute and follows the symbolic links in it. Parts
// that do not exist yet are kept as they are, after resolving what does exist.
func resolvePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	existing, rest := absolute, ""
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return absolute, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// insideStore reports whether a path, with symbolic links followed, is the store directory or inside it
func (b *Bridge) insideStore(path string) bool {
	storeDir, err := resolvePath(b.cfg.StoreDir)
	if err != nil {
		return true
	}
	target, err := resolvePath(path)
	if err != nil {
		return true
	}
	relative, err := filepath.Rel(storeDir, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	const sampleRate = 48000 // Opus granule positions always count 48 kHz samples
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}
		if i+pageSize > len(data) {
			break
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
			if headPos >= 0 && headPos+12 <= len(pageData) {
				preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
				foundOpusHead = true
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > uint64(preSkip) {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rng := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
