# In-process ISAPI talkback mixing

Opt in per camera source:

```yaml
streams:
  gate_low:
    - isapi://USER:PASS@gate/#mix=1#gain=24
    - rtsp://USER:PASS@gate/Streaming/Channels/102#backchannel=0
  gate_high:
    - isapi://USER:PASS@gate/#mix=1#gain=24
    - rtsp://USER:PASS@gate/Streaming/Channels/101#backchannel=0
```

Keep the same canonical host, credentials and mixer options in both views. Mixing
is off by default; `gain` requires `mix=1` and accepts 0–24 dB. The ISAPI scheme
handler shares one device session across low/high views. Different credentials or
options for an active shared camera are rejected. DNS aliases are not resolved for
identity: configure one hostname per physical camera. Do not mix legacy and mixed
ISAPI sources for the same device. Preserve the deployment's existing video URLs;
the example's `backchannel=0` keeps outgoing speech on the ISAPI source.

Each receiver contributes mono G.711 μ-law or A-law at 8 kHz. Up to eight active
microphones share one output clock, one camera connection and one encoder. Existing
WebRTC and RTSP clients negotiate G.711; no new ingress API, FFmpeg subprocess,
resampler, GPU or Cgo dependency is required. An unsupported format fails explicitly.

## Speech processing

- 20 ms output frames; each input's PCM ring holds at most 80 ms. This does not
  include upstream network/client/core sender queues and is not a latency guarantee.
- Oldest samples are dropped on overflow; data older than 100 ms in the mixer is
  discarded. Duplicate/out-of-order RTP is rejected, including sequence rollover.
- A 75 Hz high-pass removes DC/low rumble. Missing audio contributes PCM zero;
  silent participants never divide down another speaker's level.
- Optional per-input gain defaults to roughly -18 dBFS RMS, bounded by `gain` (24 dB
  is about 15.8x). Gain rises at 6 dB/s and falls at 60 dB/s. Digital silence does
  not raise gain; there is no hard voice-activity gate to discard whispered words.
- Summed output has an immediate peak limiter with a 120 ms release and headroom
  for G.711 quantization. The high-pass and limiter also apply with `gain=0`.

This is basic level control, not a noise suppressor or echo canceller. Amplifying
quiet speech can also amplify room noise. Existing browser microphone processing
and the gate's observed echo handling remain responsible for those effects. Actual
speech intelligibility, whisper audibility and preferred gain need real listening
acceptance. Start with conservative gain if a device clips acoustically.

## Lifecycle and fallback

A joining view attaches to the existing camera session. Leaving removes only its
receivers; the last view closes the session. Failed sessions can be replaced while
old views detach. Shutdown completes before a new session can open, preventing a
late close from terminating the replacement. Camera network operations do not hold
the registry lock for other cameras.

Mixed mode does **not** force-close a busy stock/direct talkback session. It opens
normally and reports a device rejection. Legacy mode retains its initial recovery
close. The raw audio socket is owned separately from HTTP's keep-alive pool and is
cancelable during authentication/setup. Only a successful open is closed on exit.
A device opening a channel but losing its HTTP response is inherently ambiguous;
we deliberately do not close a channel whose ownership was not confirmed.

RTSP/WebRTC negotiation success is not a device-ready acknowledgment. Existing
clients can display connected while a device's asynchronous open later fails;
logs and physical listening remain necessary for acceptance. No new readiness
protocol or wake-up preconnection is included. A client that leaves its microphone
track connected can retain the camera session even when that track is muted.

The indoor application has a separate opt-in RTSP client change with direct ISAPI
fallback when the gateway cannot be reached. Incoming gate audio and all video
remain direct. A fallback cannot mix with an unavailable gateway and must not
force-close another owner's call. Gate listening remains full duplex; existing HA
and TV local half-duplex muting for the other cameras is unchanged. Global muting
of another client's listening audio is not part of this backend change.

## Validation

Isolated tests only, with generated samples and fake localhost cameras:

```sh
go test -race ./pkg/isapi ./pkg/core ./internal/streams
go vet ./pkg/isapi ./internal/isapi ./internal/streams
go test ./pkg/isapi -run '^$' -bench BenchmarkMixer -benchmem
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/go2rtc-mixer .
```

Tests cover simultaneous voices, silent inputs, gain/limiter bounds for both laws,
RTP wrap/duplicates/late packets, bounded buffers, disconnection, participant caps,
concurrent processing, Digest authentication/cancellation, busy-device handling,
actual raw audio writes, shared views and failed-session replacement.

Apple M4 Pro sample benchmark (processing one 20 ms frame, including input decode):
1 microphone 2.5 μs; 3 microphones 4.6 μs; 8 microphones 9.2 μs. All reported
0 allocations/frame. These are isolated Mac measurements, not Frank performance
or end-to-end audio latency. Frank's CPU-only Linux amd64 artifact is cross-built;
the first live trial was rolled back. Production is on the previous build and
configuration. Further deployment, restart and rollback require explicit approval.

## Live trial follow-up: existing stream, microphone only

The first trial forwarded real panel microphone bytes through a shared low/high
ISAPI session, but concurrent connections produced intermittent panel listening/setup
failures. It was rolled back; their exact cause is still unproven.

Use `rtsp://GO2RTC:8554/gate_low?backchannel=only` for the panel microphone. No
additional stream is needed. This explicit request suppresses default playback
tracks. Put the ISAPI source first, as above, so stream selection finds the
microphone destination without first dialing the camera's RTSP playback source.
Incoming panel audio/video remain direct. Existing `backchannel=1` keeps its
playback-plus-microphone semantics for other clients.

The server also accepts receiver-only RTSP sessions (older builds reject these
with 404). An isolated test exercises the production RTSP handler, stream selection,
G.711 mixer and fake camera's raw ISAPI socket: same-view and cross-view clients,
actual RTP delivery, one camera open, leaving one participant, final cleanup and
reopening. It asserts that no playback source is contacted. All generated audio
stays in local tests. Run `go test -race ./internal/rtsp` alongside the suites above.

The revised build/configuration still need a separately approved live trial. These
local results do not establish physical audibility, device readiness, simultaneous
real speech, direct fallback during a real gateway outage, or whisper quality.


## Speech loudness target

With mixing and positive gain enabled, `#target=-12` sets the desired per-input
speech RMS to -12 dBFS, about 6 dB above the default. Accepted range: -24 to -6 dBFS.
This changes the desired speech level, not the maximum amplification (`gain`) or
final peak ceiling. Very quiet sources can still hit the gain cap before reaching
the target. Apply identical options to every view of that camera; omit the option
to preserve the original target. The limiter still controls summed peaks.

The second approved live trial (d7a0556 / panel1489364) passed eight real panel
holds, two- and three-input gate mixing, WebRTC+RTSP mixing on all three cameras,
remaining-client continuity, last-client cleanup, and a real gateway outage/direct
fallback/recovery. Panel video/listening stayed direct and HA presence/fade was
restored. HA/TV transport probes used real earlier voice recordings, not their
actual user interfaces. Alex heard the gate playback: approximately okay but could
be louder. SDK readback confirms both gate output volume controls are already10/10.
A gate-only -12 dBFS target is therefore prepared for a further approved trial;
physical whisper quality and the louder setting still need acceptance.
