package webrtc

// ABR read sessions.
//
// A path with 'abrVariants' is a virtual path: a WHEP reader of it is served
// from one of the variant paths, picked by codec (from the SDP offer) and by
// bitrate (switched at runtime). Every variant path is a normal path, so
// sourceOnDemand applies to it: a variant is pulled only while some session
// is reading it.
//
// One video track and one audio track are negotiated per session. Switching
// keeps the same SSRC and rewrites sequence numbers and timestamps so the
// browser sees one continuous stream. The switch happens at the first
// keyframe of the target variant; until then the previous variant keeps
// playing, so there is no black screen even though the RTSP / RTMP / SRT
// source cannot be asked for a keyframe.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpav1"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

const (
	abrPayloadMaxSize = 1188

	// automatic controller
	abrDownLoss         = 0.10 // fraction lost in a receiver report
	abrDownLossSevere   = 0.25
	abrUpLoss           = 0.02
	abrSwitchCooldown   = 4 * time.Second
	abrUpStableInitial  = 10 * time.Second
	abrUpStableMax      = 120 * time.Second
	abrFailedUpWindow   = 10 * time.Second
	abrAudioPTSTolerant = 500 * time.Millisecond
)

// abrDebug enables test-only PATCH fields (simCapKbps).
var abrDebug = os.Getenv("MTX_ABR_DEBUG") == "1"

func abrRandUint32() uint32 {
	var b [4]byte
	rand.Read(b[:]) //nolint:errcheck
	return binary.BigEndian.Uint32(b[:])
}

func abrTicksToDur(v int64, clockRate int64) time.Duration {
	return time.Duration(v/clockRate)*time.Second + time.Duration(v%clockRate)*time.Second/time.Duration(clockRate)
}

func abrDurToTicks(d time.Duration, clockRate int64) int64 {
	return int64(d/time.Second)*clockRate + int64(d%time.Second)*clockRate/int64(time.Second)
}

// offerCodecs returns the video codecs ("av1", "h264") present in the offer, in offer order.
func offerCodecs(offer []byte) []string {
	var desc sdp.SessionDescription
	if err := desc.Unmarshal(offer); err != nil {
		return nil
	}

	var out []string
	for _, media := range desc.MediaDescriptions {
		if media.MediaName.Media != "video" {
			continue
		}
		for _, attr := range media.Attributes {
			if attr.Key != "rtpmap" {
				continue
			}
			parts := strings.SplitN(attr.Value, " ", 2)
			if len(parts) != 2 {
				continue
			}
			var c string
			switch strings.ToUpper(strings.Split(parts[1], "/")[0]) {
			case "AV1":
				c = "av1"
			case "H264":
				c = "h264"
			default:
				continue
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// abrPickCodec chooses the codec of the session.
// Order: ?codec= query, then server preference (AV1 before H264), limited to what
// both the offer and the configured variants support.
func abrPickCodec(offer []byte, query string, variants []conf.ABRVariant) (string, error) {
	inOffer := offerCodecs(offer)

	var configured []string
	for _, v := range variants {
		if !slices.Contains(configured, v.Codec) {
			configured = append(configured, v.Codec)
		}
	}

	prefs := []string{"av1", "h264"}
	if q, err := url.ParseQuery(query); err == nil && q.Get("codec") != "" {
		prefs = []string{strings.ToLower(q.Get("codec"))}
	}

	for _, c := range prefs {
		if slices.Contains(inOffer, c) && slices.Contains(configured, c) {
			return c, nil
		}
	}

	return "", fmt.Errorf("no common codec: offer has %v, path has %v, wanted %v", inOffer, configured, prefs)
}

type abrState struct {
	Codec       string            `json:"codec"`
	Mode        string            `json:"mode"`
	Active      *conf.ABRVariant  `json:"active"`
	Pending     *conf.ABRVariant  `json:"pending"`
	Variants    []conf.ABRVariant `json:"variants"`
	Loss        float64           `json:"loss"`
	PLIs        uint64            `json:"plis"`
	NACKs       uint64            `json:"nacks"`
	Switches    int               `json:"switches"`
	LastSwitch  string            `json:"lastSwitch"`
	SwitchDelay int64             `json:"switchDelayMs"`
	UpStableFor int64             `json:"upStableForMs"`
	SimCapKbps  int               `json:"simCapKbps,omitempty"`
	SimDropped  uint64            `json:"simDropped,omitempty"`
}

type abrPatch struct {
	Mode       string `json:"mode"`   // "auto" | "manual"
	Layer      string `json:"layer"`  // variant path
	Height     int    `json:"height"` // or variant height
	Step       string `json:"step"`   // "up" | "down"
	SimCapKbps *int   `json:"simCapKbps"`
}

// abrVariantReader is the reader of one variant path.
// It is a separate defs.Reader so that the path can close it without closing the session.
type abrVariantReader struct {
	a      *abrSession
	idx    int
	path   defs.Path
	strm   *stream.Stream
	r      *stream.Reader
	closed chan struct{}
	once   sync.Once
}

// Log implements logger.Writer.
func (vr *abrVariantReader) Log(level logger.Level, format string, args ...any) {
	vr.a.s.Log(level, "[abr %s] "+format, append([]any{vr.a.variants[vr.idx].Path}, args...)...)
}

// Close implements defs.Reader.
func (vr *abrVariantReader) Close() {
	vr.once.Do(func() { close(vr.closed) })
}

// APIReaderDescribe implements defs.Reader.
func (vr *abrVariantReader) APIReaderDescribe() *defs.APIPathReader {
	return vr.a.s.APIReaderDescribe()
}

type abrSession struct {
	s        *session
	ctx      context.Context
	codec    string
	variants []conf.ABRVariant // of codec, highest bitrate first

	videoTrack *webrtc.OutboundTrack
	audioTrack *webrtc.OutboundTrack
	h264Enc    *rtph264.Encoder
	av1Enc     *rtpav1.Encoder

	chFatal chan error

	mutex   sync.Mutex
	readers map[int]*abrVariantReader
	active  int
	target  int
	mode    string

	// timeline (same for audio and video): out = variant PTS + offset[variant]
	offset        time.Duration
	ntpBase       time.Time
	videoTSBase   uint32
	audioTSBase   uint32
	lastVideoOut  time.Duration
	lastFrameDur  time.Duration
	videoStarted  bool
	audioSeq      uint16
	audioNextTS   uint32
	audioStarted  bool
	switchReqTime time.Time

	// stats / controller
	loss         float64
	highLossRuns int
	plis         uint64
	nacks        uint64
	switches     int
	lastSwitch   time.Time
	switchDelay  time.Duration
	lastUp       time.Time
	upStable     time.Duration
	goodSince    time.Time

	// link emulation (MTX_ABR_DEBUG=1)
	simCapKbps int
	simTokens  float64
	simLast    time.Time
	simDropped uint64
}

func (a *abrSession) initialize() {
	a.readers = make(map[int]*abrVariantReader)
	a.active = -1
	a.target = -1
	a.mode = "auto"
	a.upStable = abrUpStableInitial
	a.videoTSBase = abrRandUint32()
	a.audioTSBase = abrRandUint32()
	a.audioSeq = uint16(abrRandUint32())
	a.chFatal = make(chan error, 1)
}

func (a *abrSession) fatal(err error) {
	select {
	case a.chFatal <- err:
	default:
	}
}

func (a *abrSession) findFormats(desc *description.Session) (*description.Media, format.Format, *description.Media, *format.Opus) {
	var vMedia *description.Media
	var vFormat format.Format

	switch a.codec {
	case "av1":
		var f *format.AV1
		vMedia = desc.FindFormat(&f)
		if f != nil {
			vFormat = f
		}
	case "h264":
		var f *format.H264
		vMedia = desc.FindFormat(&f)
		if f != nil {
			vFormat = f
		}
	}

	var opusFormat *format.Opus
	aMedia := desc.FindFormat(&opusFormat)

	return vMedia, vFormat, aMedia, opusFormat
}

// openVariant adds a reader to a variant path. It blocks until the
// path source is ready (on-demand pull) or fails.
func (a *abrSession) openVariant(idx int) (*abrVariantReader, error) {
	ip, _, _ := net.SplitHostPort(a.s.remoteAddr)

	vr := &abrVariantReader{
		a:      a,
		idx:    idx,
		closed: make(chan struct{}),
	}

	res, err := a.s.pathManager.AddReader(defs.PathAddReaderReq{
		Author: vr,
		AccessRequest: defs.PathAccessRequest{
			Name:      a.variants[idx].Path,
			Query:     a.s.query,
			UserAgent: a.s.userAgent,
			Proto:     auth.ProtocolWebRTC,
			ID:        &a.s.uuid,
			IP:        net.ParseIP(ip),
			SkipAuth:  true, // authenticated on the ABR path
		},
	})
	if err != nil {
		return nil, err
	}

	vr.path = res.Path
	vr.strm = res.Stream

	vMedia, vFormat, aMedia, opusFormat := a.findFormats(res.Stream.OrigDesc)
	if vFormat == nil {
		res.Path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return nil, fmt.Errorf("variant '%s' has no %s track", a.variants[idx].Path, a.codec)
	}

	vr.r = &stream.Reader{Parent: vr}

	vr.r.OnData(vMedia, vFormat, func(u *unit.Unit) error {
		if u.NilPayload() {
			return nil
		}
		a.onVideo(idx, u)
		return nil
	})

	if opusFormat != nil {
		vr.r.OnData(aMedia, opusFormat, func(u *unit.Unit) error {
			a.onAudio(idx, u)
			return nil
		})
	}

	return vr, nil
}

func (a *abrSession) startVariant(vr *abrVariantReader) {
	vr.strm.AddReader(vr.r)
	vr.Log(logger.Info, "reading")

	go func() {
		var err error
		select {
		case err = <-vr.r.Error():
		case <-vr.closed:
			err = fmt.Errorf("closed")
		case <-a.ctx.Done():
			err = fmt.Errorf("terminated")
		}

		vr.strm.RemoveReader(vr.r)
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})

		a.mutex.Lock()
		cur := a.readers[vr.idx] == vr
		if cur {
			delete(a.readers, vr.idx)
		}
		wasActive := cur && a.active == vr.idx
		if cur && a.target == vr.idx {
			a.target = -1
		}
		a.mutex.Unlock()

		vr.Log(logger.Info, "stopped: %v", err)

		if wasActive && a.ctx.Err() == nil {
			a.fatal(fmt.Errorf("active variant '%s' closed: %w", a.variants[vr.idx].Path, err))
		}
	}()
}

// requestSwitch must be called with mutex held.
func (a *abrSession) requestSwitch(idx int, why string) {
	if idx < 0 || idx >= len(a.variants) || idx == a.target {
		return
	}

	// cancel a pending switch to another variant
	if a.target >= 0 {
		if vr, ok := a.readers[a.target]; ok && a.target != a.active {
			vr.Close()
		}
		a.target = -1
	}

	if idx == a.active {
		return
	}

	a.s.Log(logger.Info, "[abr] switch requested -> %s (%s)", a.variants[idx].Path, why)
	a.target = idx
	a.switchReqTime = time.Now()

	if _, ok := a.readers[idx]; ok {
		return
	}

	go func() {
		vr, err := a.openVariant(idx)

		a.mutex.Lock()
		defer a.mutex.Unlock()

		if err != nil {
			a.s.Log(logger.Warn, "[abr] cannot open '%s': %v", a.variants[idx].Path, err)
			if a.target == idx {
				a.target = -1
			}
			return
		}

		if a.target != idx || a.ctx.Err() != nil {
			// superseded while opening
			vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
			return
		}

		a.readers[idx] = vr
		a.startVariant(vr)
	}()
}

// onVideo is called by the reader goroutine of a variant.
func (a *abrSession) onVideo(idx int, u *unit.Unit) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	var randomAccess bool
	switch pl := u.Payload.(type) {
	case unit.PayloadAV1:
		randomAccess = av1.IsRandomAccess2(pl)
	case unit.PayloadH264:
		randomAccess = h264.IsRandomAccess(pl)
	}

	pts := abrTicksToDur(u.PTS, 90000)

	if idx == a.target && randomAccess {
		prev := a.active
		if a.videoStarted {
			a.offset = a.lastVideoOut + a.lastFrameDur - pts
		} else {
			a.offset = -pts
			a.ntpBase = time.Now()
		}
		a.active = idx
		a.target = -1
		a.switches++
		a.lastSwitch = time.Now()
		a.switchDelay = time.Since(a.switchReqTime)
		a.audioStarted = false // re-anchor audio on the new variant
		a.goodSince = time.Now()
		a.highLossRuns = 0

		a.s.Log(logger.Info, "[abr] switched to %s at keyframe after %v", a.variants[idx].Path, a.switchDelay)

		if prev >= 0 {
			if vr, ok := a.readers[prev]; ok {
				go vr.Close()
			}
		}
	}

	if idx != a.active {
		return
	}

	out := pts + a.offset
	if a.videoStarted {
		if d := out - a.lastVideoOut; d > 0 && d < time.Second {
			a.lastFrameDur = d
		}
	} else {
		a.lastFrameDur = 40 * time.Millisecond
	}
	a.lastVideoOut = out
	a.videoStarted = true

	var packets []*rtp.Packet
	var err error
	switch pl := u.Payload.(type) {
	case unit.PayloadAV1:
		packets, err = a.av1Enc.Encode(pl)
	case unit.PayloadH264:
		packets, err = a.h264Enc.Encode(pl)
	}
	if err != nil {
		return
	}

	ts := a.videoTSBase + uint32(abrDurToTicks(out, 90000))
	ntp := a.ntpBase.Add(out)

	for _, pkt := range packets {
		pkt.Timestamp = ts
		if !a.simAllow(len(pkt.Payload) + 12) {
			continue
		}
		a.videoTrack.WriteRTPWithNTP(pkt, ntp) //nolint:errcheck
	}
}

func (a *abrSession) onAudio(idx int, u *unit.Unit) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if idx != a.active || !a.videoStarted || a.audioTrack == nil {
		return
	}

	out := abrTicksToDur(u.PTS, 48000) + a.offset
	if out < 0 {
		return
	}
	ts := a.audioTSBase + uint32(abrDurToTicks(out, 48000))

	// keep timestamps contiguous (Chrome wants precise ones), re-anchor on drift or switch
	tol := uint32(abrDurToTicks(abrAudioPTSTolerant, 48000))
	if d := int32(ts - a.audioNextTS); !a.audioStarted || d > int32(tol) || d < -int32(tol) {
		a.audioNextTS = ts
		a.audioStarted = true
	}

	for _, orig := range u.RTPPackets {
		pkt := &rtp.Packet{
			Header:  orig.Header,
			Payload: orig.Payload,
		}
		pkt.SequenceNumber = a.audioSeq
		a.audioSeq++
		pkt.Timestamp = a.audioNextTS
		a.audioNextTS += uint32(opus.PacketDuration2(pkt.Payload))

		if !a.simAllow(len(pkt.Payload) + 12) {
			continue
		}
		a.audioTrack.WriteRTPWithNTP(pkt, a.ntpBase.Add(out)) //nolint:errcheck
	}
}

// simAllow is a token bucket emulating a capped link. Must be called with mutex held.
func (a *abrSession) simAllow(n int) bool {
	if a.simCapKbps <= 0 {
		return true
	}
	now := time.Now()
	rate := float64(a.simCapKbps) * 1000 / 8 // bytes/s
	burst := rate * 0.1
	a.simTokens += now.Sub(a.simLast).Seconds() * rate
	a.simLast = now
	if a.simTokens > burst {
		a.simTokens = burst
	}
	if a.simTokens < float64(n) {
		a.simDropped++
		return false
	}
	a.simTokens -= float64(n)
	return true
}

// onVideoRTCP runs the automatic controller on receiver reports.
func (a *abrSession) onVideoRTCP(pkts []rtcp.Packet) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.PictureLossIndication:
			a.plis++

		case *rtcp.TransportLayerNack:
			a.nacks++

		case *rtcp.ReceiverReport:
			for _, rep := range p.Reports {
				if rep.SSRC != 0 {
					a.onLossReport(float64(rep.FractionLost) / 256)
				}
			}
		}
	}
}

func (a *abrSession) onLossReport(loss float64) {
	a.loss = loss
	now := time.Now()

	if loss >= abrDownLoss {
		a.highLossRuns++
		a.goodSince = time.Time{}
	} else {
		a.highLossRuns = 0
		if loss < abrUpLoss && a.goodSince.IsZero() {
			a.goodSince = now
		} else if loss >= abrUpLoss {
			a.goodSince = time.Time{}
		}
	}

	if a.mode != "auto" || a.active < 0 || a.target >= 0 || now.Sub(a.lastSwitch) < abrSwitchCooldown {
		return
	}

	// down: two lossy reports in a row, or one very lossy one
	if a.active < len(a.variants)-1 && (a.highLossRuns >= 2 || loss >= abrDownLossSevere) {
		if !a.lastUp.IsZero() && now.Sub(a.lastUp) < abrFailedUpWindow {
			// the last up-switch did not hold: wait longer before the next try
			a.upStable = min(a.upStable*2, abrUpStableMax)
		}
		a.lastUp = time.Time{}
		a.requestSwitch(a.active+1, fmt.Sprintf("loss %.0f%%", loss*100))
		return
	}

	// up: no loss for upStable. GCC is not used here: there is no send-side
	// estimate in MediaMTX, and an estimate cannot see headroom above the current rate anyway.
	if a.active > 0 && !a.goodSince.IsZero() && now.Sub(a.goodSince) >= a.upStable {
		if a.lastUp.IsZero() || now.Sub(a.lastUp) >= abrFailedUpWindow {
			// previous up held: reset backoff
			if !a.lastUp.IsZero() {
				a.upStable = abrUpStableInitial
			}
		}
		a.lastUp = now
		a.requestSwitch(a.active-1, fmt.Sprintf("no loss for %v", a.upStable))
	}
}

func (a *abrSession) state() *abrState {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.stateLocked()
}

func (a *abrSession) stateLocked() *abrState {
	st := &abrState{
		Codec:       a.codec,
		Mode:        a.mode,
		Variants:    a.variants,
		Loss:        a.loss,
		PLIs:        a.plis,
		NACKs:       a.nacks,
		Switches:    a.switches,
		SwitchDelay: a.switchDelay.Milliseconds(),
		UpStableFor: a.upStable.Milliseconds(),
		SimCapKbps:  a.simCapKbps,
		SimDropped:  a.simDropped,
	}
	if a.active >= 0 {
		v := a.variants[a.active]
		st.Active = &v
	}
	if a.target >= 0 {
		v := a.variants[a.target]
		st.Pending = &v
	}
	if !a.lastSwitch.IsZero() {
		st.LastSwitch = a.lastSwitch.Format(time.RFC3339Nano)
	}
	return st
}

func (a *abrSession) patch(body []byte) (*abrState, error) {
	var p abrPatch
	if len(body) != 0 {
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	switch p.Mode {
	case "":
	case "auto", "manual":
		a.mode = p.Mode
		a.goodSince = time.Time{}
	default:
		return nil, fmt.Errorf("invalid mode '%s'", p.Mode)
	}

	if p.SimCapKbps != nil {
		if !abrDebug {
			return nil, fmt.Errorf("simCapKbps requires MTX_ABR_DEBUG=1")
		}
		a.simCapKbps = *p.SimCapKbps
		a.simLast = time.Now()
		a.simTokens = 0
	}

	target := -1
	switch {
	case p.Layer != "":
		for i, v := range a.variants {
			if v.Path == p.Layer {
				target = i
			}
		}
		if target < 0 {
			return nil, fmt.Errorf("unknown layer '%s'", p.Layer)
		}

	case p.Height != 0:
		for i, v := range a.variants {
			if v.Height == p.Height {
				target = i
			}
		}
		if target < 0 {
			return nil, fmt.Errorf("no layer with height %d", p.Height)
		}

	case p.Step == "down":
		target = min(max(a.active, 0)+1, len(a.variants)-1)

	case p.Step == "up":
		target = max(a.active-1, 0)

	case p.Step != "":
		return nil, fmt.Errorf("invalid step '%s'", p.Step)
	}

	if target >= 0 {
		// an explicit choice turns off the automatic controller
		if p.Mode == "" {
			a.mode = "manual"
		}
		a.requestSwitch(target, "PATCH")
	}

	return a.stateLocked(), nil
}

// runReadABR serves a read session of a path that has abrVariants.
func (s *session) runReadABR(
	errGroupCtx context.Context,
	req *initialRequestReq,
	pathConf *conf.Path,
) (int, error) {
	codec, err := abrPickCodec(s.offer, s.query, pathConf.ABRVariants)
	if err != nil {
		return 406, err
	}

	a := &abrSession{
		s:     s,
		ctx:   errGroupCtx,
		codec: codec,
	}
	a.initialize()

	for _, v := range pathConf.ABRVariants {
		if v.Codec == codec {
			a.variants = append(a.variants, v)
		}
	}
	slices.SortStableFunc(a.variants, func(x, y conf.ABRVariant) int { return y.Bitrate - x.Bitrate })

	start := 0
	if q, err2 := url.ParseQuery(s.query); err2 == nil {
		switch h := q.Get("start"); h {
		case "", "high":
		case "low":
			start = len(a.variants) - 1
		default:
			for i, v := range a.variants {
				if fmt.Sprint(v.Height) == h || v.Path == h {
					start = i
				}
			}
		}
	}

	s.Log(logger.Info, "[abr] codec %s (offer has %v), %d variants, starting with %s",
		codec, offerCodecs(s.offer), len(a.variants), a.variants[start].Path)

	// cold start: the first variant is opened before the answer, as in a normal read
	vr, err := a.openVariant(start)
	if err != nil {
		if _, ok := errors.AsType[*defs.PathNoStreamAvailableError](err); ok {
			return 404, err
		}
		return 400, err
	}

	_, _, _, opusFormat := a.findFormats(vr.strm.OrigDesc)

	switch codec {
	case "av1":
		a.videoTrack = &webrtc.OutboundTrack{
			Caps: pwebrtc.RTPCodecCapability{MimeType: pwebrtc.MimeTypeAV1, ClockRate: 90000},
		}
		a.av1Enc = &rtpav1.Encoder{PayloadType: 105, PayloadMaxSize: abrPayloadMaxSize}
		err = a.av1Enc.Init()

	case "h264":
		a.videoTrack = &webrtc.OutboundTrack{
			Caps: pwebrtc.RTPCodecCapability{
				MimeType:    pwebrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			},
		}
		a.h264Enc = &rtph264.Encoder{PayloadType: 96, PayloadMaxSize: abrPayloadMaxSize, PacketizationMode: 1}
		err = a.h264Enc.Init()
	}
	if err != nil {
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return 500, err
	}
	a.videoTrack.OnRTCP = a.onVideoRTCP

	if opusFormat != nil {
		a.audioTrack = &webrtc.OutboundTrack{
			Caps: pwebrtc.RTPCodecCapability{
				MimeType:    pwebrtc.MimeTypeOpus,
				ClockRate:   48000,
				Channels:    2,
				SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
			},
		}
	}

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return 500, err
	}

	pc := &webrtc.PeerConnection{
		Net:                          s.net,
		ICEUDPMux:                    s.iceUDPMux,
		ICETCPMux:                    s.iceTCPMux,
		SupportsIPv6:                 s.supportsIPv6,
		ICEServers:                   iceServers,
		IPsFromInterfaces:            s.ipsFromInterfaces,
		IPsFromInterfacesList:        s.ipsFromInterfacesList,
		IPsFromInterfacesExcludeList: s.ipsFromInterfacesExcludeList,
		AdditionalHosts:              s.additionalHosts,
		STUNGatherTimeout:            time.Duration(s.stunGatherTimeout),
		Publish:                      true,
		Log:                          s,
	}
	pc.OutboundTracks = append(pc.OutboundTracks, a.videoTrack)
	if a.audioTrack != nil {
		pc.OutboundTracks = append(pc.OutboundTracks, a.audioTrack)
	}

	err = pc.Start()
	if err != nil {
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return 400, err
	}

	defer func() {
		s.mutex.Lock()
		pc.Close()
		s.mutex.Unlock()
	}()

	answer, err := pc.CreateFullAnswer(whipOffer(s.offer), false)
	if err != nil {
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return 400, err
	}

	req.res <- initialRequestRes{
		answer: []byte(answer.SDP),
	}

	go s.readRemoteCandidates(s.offer, pc)

	err = pc.WaitUntilConnected(time.Duration(s.handshakeTimeout))
	if err != nil {
		vr.path.RemoveReader(defs.PathRemoveReaderReq{Author: vr})
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.abr = a
	s.mutex.Unlock()

	s.Log(logger.Info, "is reading from ABR path '%s' via '%s'", s.pathName, a.variants[start].Path)

	a.mutex.Lock()
	a.target = start
	a.switchReqTime = time.Now()
	a.readers[start] = vr
	a.startVariant(vr)
	a.mutex.Unlock()

	select {
	case err = <-a.chFatal:
		return 0, err
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")
	case <-errGroupCtx.Done():
		return 0, fmt.Errorf("terminated")
	}
}
