# Per-stream G.711 latency

The deployed legacy WebRTC path repacks G.711 into1024-byte packets. At8kHz mono this is128ms of audio.
Audio/video synchronization can hold video behind that packet buffer, even with a small requested browser
jitter-buffer target. Preserve the legacy path for existing cameras that depend on its audio framing.

Opt selected stream names into20ms/160-byte G.711 output:

```yaml
webrtc:
  g711_low_latency: [mi360_low, mi360_high]
```

Both WebSocket and HTTP/WHEP consumers use this policy. Browser microphone/backchannel producers do not.
8kHz G.711 remains encoded as received; this does not synthesize audio, resample, transcode or change
camera capture settings. All complete160-sample packets drain in order; incomplete tails are retained.
Only the initial packet marks the continuous talkspurt. Other clock rates and unlisted streams retain
legacy1024 framing, zero-timestamp behavior where selected, and markers.

The private real-camera preview on2026-10-27 measured fullscreen video jitter-buffer delay189.4→74.7ms;
real brightness-change-to-display samples344/388→240/302ms through an additional local RTSP relay hop.
The browser received160-byte PCMU packets, zero packet loss and no video drops/freezes in those short
samples. These are Mac/Chromium samples, not a guarantee for every client/network. Receiver target0 alone
had not materially improved the original128ms audio path. RFC3551 recommends20ms default packetization.

Tests cover sample preservation, chunk boundaries, timestamp/sequence continuity, payload ownership,
initial-only marking and unchanged legacy framing. The source baseline is the exact deployed33526a0
including talkback/mixer/reclaim fixes. No public branch push or PR is authorized without Alex reviewing
the exact local diff and explicitly approving publication. Device deployment is separate from publication.
