package basispoints

import (
	"crypto/sha256"
	"encoding/base64"
	"mime"
	"net/url"
	"strings"
	"sync"
)

const maxAttachmentCacheEntries = 512

type cachedAttachment struct {
	key    [sha256.Size]byte
	fileID string
}

type pendingAttachment struct {
	done   chan struct{}
	fileID string
	err    error
}

type attachmentCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*listElement
	order   listElementList
	pending map[[sha256.Size]byte]*pendingAttachment
}

// listElement/listElementList are a tiny LRU list so the transport package does
// not need to expose image bytes or bring in another cache dependency.
type listElement struct {
	value cachedAttachment
	prev  *listElement
	next  *listElement
}

type listElementList struct {
	front *listElement
	back  *listElement
	len   int
}

func (l *listElementList) moveToFront(element *listElement) {
	if element == nil || l.front == element {
		return
	}
	l.unlink(element)
	l.pushFront(element)
}

func (l *listElementList) pushFront(element *listElement) {
	element.prev = nil
	element.next = l.front
	if l.front != nil {
		l.front.prev = element
	} else {
		l.back = element
	}
	l.front = element
	l.len++
}

func (l *listElementList) unlink(element *listElement) {
	if element.prev != nil {
		element.prev.next = element.next
	} else {
		l.front = element.next
	}
	if element.next != nil {
		element.next.prev = element.prev
	} else {
		l.back = element.prev
	}
	l.len--
}

func (l *listElementList) remove(element *listElement) {
	if element == nil {
		return
	}
	l.unlink(element)
	element.prev = nil
	element.next = nil
}

func (c *attachmentCache) getOrUpload(key [sha256.Size]byte, upload func() (string, error)) (string, error) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.order.moveToFront(entry)
		fileID := entry.value.fileID
		c.mu.Unlock()
		return fileID, nil
	}
	if pending := c.pending[key]; pending != nil {
		c.mu.Unlock()
		<-pending.done
		return pending.fileID, pending.err
	}
	if c.pending == nil {
		c.pending = make(map[[sha256.Size]byte]*pendingAttachment)
	}
	pending := &pendingAttachment{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()

	fileID, err := upload()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[[sha256.Size]byte]*listElement)
		}
		entry := &listElement{value: cachedAttachment{key: key, fileID: fileID}}
		c.entries[key] = entry
		c.order.pushFront(entry)
		if c.order.len > maxAttachmentCacheEntries {
			oldest := c.order.back
			delete(c.entries, oldest.value.key)
			c.order.remove(oldest)
		}
	}
	pending.fileID, pending.err = fileID, err
	close(pending.done)
	return fileID, err
}

type inlineImage struct {
	mediaType string
	data      []byte
}

func decodeInlineImage(dataURL string) (inlineImage, error) {
	metadata, encoded, found := strings.Cut(dataURL[5:], ",")
	if !found {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL is missing its data separator")
	}
	isBase64 := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if isBase64 {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL must declare an image media type")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL has invalid percent encoding")
	}
	var data []byte
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	} else {
		data = []byte(decoded)
	}
	if err != nil || len(data) == 0 {
		return inlineImage{}, fail(400, "invalid_image", "input_image data URL contains empty or invalid image data")
	}
	return inlineImage{mediaType: mediaType, data: data}, nil
}

func attachmentURL(responsesURL string) (string, error) {
	base, err := url.Parse(strings.TrimRight(responsesURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fail(500, "invalid_config", "cannot derive attachments endpoint from responses_url")
	}
	return base.ResolveReference(&url.URL{Path: "attachments"}).String(), nil
}

func attachmentErrorMessage(raw []byte, c credential, image inlineImage) string {
	message := string(raw)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email, base64.StdEncoding.EncodeToString(image.data), string(image.data)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return redactTokenMessage(errorMessage([]byte(message)))
}
