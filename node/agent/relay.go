package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A paired node (join --keep) gets its offers through a relay instead of
// copy and paste: an ntfy server (https://ntfy.sh by default), which both
// sides reach with outgoing HTTPS only. A tab posts an offer to the topic
// of the node; the agent posts its answer to the reply topic of the offer.
// Both are sealed with AES-GCM and the key of the token, so the relay sees
// neither the SDP nor the addresses in it. web/src/relay.js is the other
// side.

// relayMsg is what a tab sends: its offer, the topic for the answer, and
// when it sent it (milliseconds since the epoch).
type relayMsg struct {
	SDP   string `json:"sdp"`
	Reply string `json:"reply,omitempty"`
	Time  int64  `json:"t,omitempty"`
}

// offerMaxAge: an older offer is a replay, or the tab gave up on it.
const offerMaxAge = 2 * time.Minute

var topicRE = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

type relay struct {
	url   string
	topic string
	aead  cipher.AEAD
	http  *http.Client
	seen  map[string]bool // the reply topics of the offers answered
}

func newRelay(url, topic, key string) (*relay, error) {
	if !topicRE.MatchString(topic) {
		return nil, fmt.Errorf("invalid relay topic %q", topic)
	}
	k, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(k) != 32 {
		return nil, errors.New("invalid relay key")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &relay{url: strings.TrimRight(url, "/"), topic: topic, aead: aead, http: &http.Client{}, seen: map[string]bool{}}, nil
}

// seal deflates and encrypts v: base64url(nonce | ciphertext).
func (r *relay) seal(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(raw)
	w.Close()
	nonce := make([]byte, r.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(r.aead.Seal(nonce, nonce, buf.Bytes(), nil)), nil
}

func (r *relay) open(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	n := r.aead.NonceSize()
	if len(b) < n {
		return errors.New("short message")
	}
	plain, err := r.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(plain)), 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (r *relay) publish(ctx context.Context, topic, msg string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/"+topic, strings.NewReader(msg))
	if err != nil {
		return err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("relay: %s", resp.Status)
	}
	return nil
}

// serve waits for offers on the topic of the node and answers each one
// with answer. It subscribes again when the stream ends.
func (r *relay) serve(ctx context.Context, answer func(sdp string) (string, error)) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := r.subscribe(ctx, answer)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("relay: %v, again in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

func (r *relay) subscribe(ctx context.Context, answer func(sdp string) (string, error)) error {
	// since=30s: the offers of the last seconds too. A tab may have sent
	// one while this container started, and it waits 20 s for the answer.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url+"/"+r.topic+"/json?since=30s", nil)
	if err != nil {
		return err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribing: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var ev struct {
			Event   string `json:"event"`
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "message" {
			continue // open, keepalive
		}
		var m relayMsg
		if err := r.open(ev.Message, &m); err != nil {
			log.Printf("relay: ignoring a message that is not for this node: %v", err)
			continue
		}
		age := time.Since(time.UnixMilli(m.Time))
		if m.SDP == "" || !topicRE.MatchString(m.Reply) || age > offerMaxAge || age < -offerMaxAge || r.seen[m.Reply] {
			continue
		}
		r.seen[m.Reply] = true
		go func() {
			sdp, err := answer(m.SDP)
			if err != nil {
				log.Printf("relay: answering an offer: %v", err)
				return
			}
			msg, err := r.seal(relayMsg{SDP: sdp})
			if err == nil {
				err = r.publish(ctx, m.Reply, msg)
			}
			if err != nil {
				log.Printf("relay: sending the answer: %v", err)
			}
		}()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
