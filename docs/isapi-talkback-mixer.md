# In-process ISAPI talkback mixing

Opt in per camera source:

```yaml
streams:
  gate_low:
    - rtsp://USER:PASS@gate/Streaming/Channels/102#backchannel=0
    - isapi://USER:PASS@gate/#mix=1#gain=24
  gate_high:
    - rtsp://USER:PASS@gate/Streaming/Channels/101#backchannel=0
    - isapi://USER:PASS@gate/#mix=1#gain=24
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
- Optional per-input gain targets roughly -18 dBFS RMS, bounded by `gain` (24 dB
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
no binary, configuration, benchmark workload or service change has been applied
there for this feature. Deployment, restart and rollback require explicit approval.

## Live trial follow-up: dedicated microphone stream

The first trial successfully forwarded real panel microphone bytes through a shared
low/high ISAPI session, but concurrent connections produced intermittent panel
listening/setup failures. That trial was rolled back. A locally reproduced RTSP
limitation rejects a microphone-only session with 404 because the server required
playback senders even when a backchannel receiver exists. The server fix accepts
receiver-only sessions; its isolated DESCRIBE/SETUP/PLAY regression fails before
and passes after the change.

For the panel, add a dedicated stream with the **same** ISAPI source/options:

```yaml
streams:
  gate_talk: isapi://USER:PASS@gate/#mix=1#gain=24
```

Use `rtsp://GO2RTC:8554/gate_talk?backchannel=1` for its outgoing microphone. This
shares the ISAPI session with HA/TV low/high views without creating an unnecessary
camera RTSP playback connection for the panel's microphone. Incoming panel audio
remains direct. This removes a known dependency; a connection-limit hypothesis for
the observed failure is not yet proven. The revised route needs separate live
acceptance and deployment approval; no second candidate deployment has occurred.
