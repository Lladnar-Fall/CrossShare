# CrossShare

Instant, cross-device clipboard and file sharing across laptops, desktops, VMs, and mobile — no manual transfers, email hacks, or broken VM guest tools.

## What it does

Copy something on one device, paste it on another — usually in under 100ms. Drop a file in a folder on one machine, it shows up on the others automatically. Works online and offline, with queued delivery for devices that are asleep or disconnected.

## Architecture

CrossShare is a three-part relay system:

| Component | Role |
|---|---|
| **Relay Server** (cloud, Go) | Routes messages between devices over persistent WebSocket connections. Pure relay — no business logic about clipboards or files, it just forwards JSON between devices owned by the same user. |
| **Background Agent** | Native process on Windows, Mac, Linux, or a VM. Polls the OS clipboard, watches local drop folders, and talks to the relay server. |
| **IDE Plugin & Mobile App** | Control surface inside IntelliJ and on mobile. Lets you send snippets directly from the editor or a phone, and hosts a local REST API for the agent. |

```
┌─────────────────────┐                                          ┌─────────────────────┐
│   DEVICE A           │                                         │   DEVICE B           │
│  Clipboard Monitor   │──push (WebSocket)──►┌──────────┐◄───────│  Clipboard Writer     │
│  (50ms poll)         │                      │  RELAY   │        │                      │
│  File Watcher        │──blob upload (HTTP)─►│  SERVER  │◄──────│  File Writer          │
│  (fsnotify)          │                      │ (SQLite  │        │                      │
│  Local API :9876     │                      │  WAL)    │        │  Local API :9876      │
│  IDE Plugin          │                      └──────────┘        │  IDE Plugin           │
└─────────────────────┘                                          └─────────────────────┘
```

## How a copy/paste travels (<100ms)

1. **Detection** — the agent polls the OS clipboard every 50ms, catches `Ctrl+C`, and hashes the content.
2. **Transmission** — the content is packaged into JSON and sent up the open WebSocket.
3. **Instant relay** — the server forwards the message to the user's other online devices immediately, *before* writing anything to the database (the DB write happens asynchronously).
4. **OS injection** — the receiving agent decodes the payload and writes it straight into the target OS clipboard.
5. **Paste** — the user hits `Ctrl+V` and the content is there.

## Setup & authentication

- **Login** — the device authenticates with account credentials and receives a unique device token.
- **Hashed token storage** — the device keeps the raw token; the server stores only a SHA256 fingerprint. Even if the database is stolen, no one can impersonate a device from it.
- **Persistent connection** — a bidirectional WebSocket stays open 24/7 to avoid connection setup delay.
- **Keepalive** — a ping every 10 seconds stops cloud proxies (Cloudflare, Railway, etc.) from silently dropping idle connections.

## Loop protection

Without safeguards, a copy on Device A would echo forever: A sends to B, B writes to its clipboard, B's poller sees "new" text and sends it back to A, and so on.

CrossShare prevents this with two hash guards:

- **`lastSentHash`** — set when a device sends text, so it never re-sends what it just sent.
- **`lastAppliedHash`** — set when a device receives and writes text, so its own poller ignores that text on the next check.

```
Copy "hello" on Device A:
  A: lastSentHash = SHA256("hello")       → sends to server
  B: receives it, writes "hello" to clipboard
  B: lastAppliedHash = SHA256("hello")
  B: poller reads "hello" → matches lastAppliedHash → skipped, no echo
```

## File transfer

Transfer method scales with file size:

- **Inline streaming (< 1MB)** — small files and snippets are gzipped and sent directly over the WebSocket.
- **Blob storage (> 1MB)** — larger files are uploaded over HTTP to the server (`/api/blobs`); the receiver gets a signal token and streams the file down separately.
- **Drop folders** — files placed in `~/CrossShare/send` on one device land automatically in `~/CrossShare/received` on the others.
- Directories are zipped in memory before sending. Files that don't compress well (images, zips) are sent uncompressed.

## Offline support (store-and-forward)

- **Pending queue** — items sent to an offline device are held on the server with expiration timers: 30 minutes for text, 24 hours for files.
- **Reconnection catch-up** — when a device comes back online, the server delivers everything that was queued for it.
- **Safe inbox** — queued/backlog items land in a dedicated inbox in the IDE plugin instead of silently overwriting whatever's currently on the clipboard.

## Performance notes

| Optimization | Effect |
|---|---|
| Async DB writes on the delivery path | Cuts delivery latency from ~6s to ~100ms |
| SQLite WAL mode | Writes drop from ~2s to ~1ms each (avoids full fsync per insert) |
| `synchronous=NORMAL` | Fewer fsyncs per transaction |
| 10s WebSocket keepalive | Stops idle connections being dropped by cloud proxies |
| Gzip on file transfer | Cuts transfer size 60–90% for text/code |
| 1MB inline/blob threshold | Small items stay fast over WebSocket; large ones stream over HTTP |
| Buffered send channel | Clipboard copies never block waiting on a slow network |
| Native OS clipboard APIs | Avoids subprocess overhead that caused lag under WSL |

## Security model

| Layer | Mechanism |
|---|---|
| User passwords | bcrypt-hashed |
| Device tokens | SHA256 hash stored server-side; raw token lives only on the device |
| WebSocket auth | First message on a new connection must carry a valid device ID + token |
| Local API | Requires a bearer token, so other apps on the machine can't read your clipboard data |
| Blob access | Server checks a blob belongs to the requesting user's items before serving it |

## Requirements

- A relay server instance (cloud-hosted)
- The background agent running on each device (Windows, Mac, Linux, or VM)
- The IDE plugin (IntelliJ) and/or mobile app for direct control

---

*This README was assembled from the CrossShare architecture guide and internal technical documentation.*
