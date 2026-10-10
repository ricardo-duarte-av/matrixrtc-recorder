# matrixrtc-recorder

A Matrix bot that joins MatrixRTC (Element Call) calls on request and records every participant's
audio and video, end-to-end encrypted calls included. No browser and no Element Call code: Matrix
through [mautrix-go], media through [LiveKit's Go SDK], protocol details ported from xmuks.

**Status: working proof of concept**, tested live on a Synapse + LiveKit deployment with xmuks
clients and Element Web in the call.

## What works

- **Rooms:** unencrypted and end-to-end encrypted (Megolm for room events, Olm for call keys).
- **Calls:** joins as a real participant with sticky (MSC4354) or legacy state memberships,
  whichever the call uses, kept alive by a delayed leave (MSC4140) so a crash leaves within ~18 s.
- **E2EE media:** per-participant keys from `io.element.call.encryption_keys` to-device events,
  LiveKit's HKDF derivation, key rotation when people join or leave.
- **Codecs:** Opus audio; H.264 and VP8 video (whole access units reassembled before decryption).
- **Multi-SFU and federation:** connects to every LiveKit server the members publish on, e.g. a
  participant from another homeserver with its own SFU.
- **Faithful timing:** audio silences (DTX) and mutes are filled with silence; video keeps its RTP
  timestamps; `meta.json` gives each track's first-frame wall-clock time for lining them up.
- **Commands:** `!record start | stop | status | help`, start/stop limited by power level
  (default 50, moderator); start/stop announced in the room; recording stops when everyone leaves.
- **Deployment:** Docker image for amd64/arm64 on GHCR, sample `docker-compose.yaml`, clean
  shutdown (leaves calls and finishes files on SIGTERM).

## How it works

1. Invite the bot to a room; it joins.
2. A user with power level ≥ `min_power_level` (default 50, moderator) sends `!record start`.
3. The bot joins the call as a real participant: it publishes its own membership (sticky MSC4354
   event or legacy state event, whichever the call uses), with a delayed leave (MSC4140) it keeps
   restarting so a crash removes it from the call within ~18 s.
4. Because it's a member, the other participants send it their media keys
   (`io.element.call.encryption_keys`, Olm-encrypted to-device).
5. It connects to every LiveKit SFU the members publish on, subscribes to all tracks, decrypts the
   frames and writes one file per track.
6. `!record stop`, or everyone leaving, ends it. The bot announces start and stop in the room.

Output, per recording:

```
recordings/<room>/<UTC start>/
  alice_example.org_ABCDEF_TR_xxxx.ogg        # Opus
  alice_example.org_ABCDEF_TR_yyyy.h264.ivf   # H.264 (Annex B in IVF; .ivf alone is VP8)
  meta.json                                   # who's who, codecs, first-frame wall-clock times
```

Turn an H.264 track into MP4 with `ffmpeg -i x.h264.ivf -c copy x.mp4`.

Mix them afterwards, e.g. audio only:

```sh
ffmpeg -i a.ogg -i b.ogg -filter_complex amix=inputs=2:normalize=0 call.ogg
```

Use `first_frame_at` from `meta.json` to offset tracks that started at different times (`-itsoffset`
or `adelay`).

## Running with Docker

Images for amd64 and arm64 are published to GHCR on every push to `main` (`latest`, `sha-…`) and
for version tags (`v1.2.3` → `1.2.3`, `1.2`).

```sh
mkdir -p data && cp config.example.yaml data/config.yaml   # fill it in
sudo chown -R 1000:1000 data                               # the container runs as uid 1000
docker compose up -d                                       # see docker-compose.yaml
docker compose logs -f
```

Everything lives in `/data`: `config.yaml`, the encryption database and `recordings/` (paths in the
config are relative to it). Back up `data/`: losing the database means a new bot device. On
`docker compose down` the bot leaves the calls it's recording and finishes their files first.

## Running from source

Needs Go ≥ 1.26.8 (`GOTOOLCHAIN=go1.26.8` fetches it). Olm is mautrix's pure-Go implementation
(`-tags goolm`), so no libolm is needed.

```sh
make build
cp config.example.yaml config.yaml   # fill in homeserver, user, password, pickle_key
./matrixrtc-recorder -config config.yaml
```

Commands: `!record start`, `!record stop`, `!record status`, `!record help`.

### Testing against a real call

1. Run the bot with `log_level: debug`.
2. Start an Element Call (or xmuks) call in an encrypted room the bot is in, with two participants.
3. `!record start`. In the log, look for `Connected to SFU`, `Received media key` per participant,
   `Recording track`, then `First frame written`.
4. Talk for a bit, `!record stop`, and play the `.ogg`/`.ivf` files with `ffplay`.

If `Can't decrypt frame` keeps repeating for a participant, their key never arrived. Check that
they really sent one to the bot's device: their client must see the bot's device (some clients
skip unverified devices).

## TODO

- [ ] **Web server** to download recordings, with access limited to members of the room.
- [ ] **Mixing:** produce one file per recording (videos side by side, audio mixed), as an option.
- [ ] **Revoked login:** log in again (or exit loudly) when the bot's session is signed out; today
      it keeps syncing but can't act.
- [ ] **Reconnection** to an SFU whose connection drops mid-recording.
- [ ] **Video quality:** make sure the high simulcast layer is selected (`SetVideoDimensions`).
- [ ] **`/commands`:** publish command descriptions for clients that support bot slash commands.
- [ ] **Cross-signing** for the bot's device, for clients that only send keys to verified devices.
- [ ] Keys sent as **room events** (older matrix-js-sdk transport).
- [ ] **VP9/AV1**, once their clear-header rules are checked.
- [ ] MSC4195 `m.livekit` transports (token from the homeserver instead of lk-jwt-service).
- [ ] Retention: delete recordings after a configurable time.
- [ ] Cleanups: duplicate "Joined room" log on start, quieter debug logging.

### Good to know

- A sender's first frames after the bot joins can be undecryptable for about a second: joining
  makes the others rotate their key (so the bot can't read what came before), and they switch to
  the new key shortly after sending it.
- A track that never produces a frame (muted throughout) gets no file, only an entry in
  `meta.json`.
- Clients must switch to their rotated key. An xmuks build that didn't (LiveKit Android only sets
  the sender's key index when a track is published) left everyone joining later, the bot included,
  unable to decrypt it.

## Layout

| Path | What |
|---|---|
| `internal/rtc` | MatrixRTC: memberships in both formats, SFU identities, media key ring, JWT, own membership lifecycle |
| `internal/recorder` | LiveKit connections, frame decryption (VP8 is ours, Opus/H.264 the SDK's), access-unit reassembly, Ogg/IVF writers |
| `internal/bot` | Matrix client, E2EE, invites, commands, power levels, recording lifecycle |

[mautrix-go]: https://github.com/mautrix/go
[LiveKit's Go SDK]: https://github.com/livekit/server-sdk-go
