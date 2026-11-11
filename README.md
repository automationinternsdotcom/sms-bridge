<img src="assets/icon.svg" width="64" height="64" alt="">

# sms-bridge

Copy every text from your Android phone into a Google Sheet within seconds. That covers SMS, MMS and RCS, including sent and end-to-end-encrypted RCS.

It runs on a Mac as an extra **Google Messages for Web** device, using [mautrix-gmessages](https://github.com/mautrix/gmessages)' `libgm`. That makes it a reverse-engineered, unofficial protocol: it can break whenever Google changes something.

## Why this approach

On current Android (16/17), apps on the phone can't see your sent RCS or your encrypted RCS. Google Messages no longer writes sent RCS to the shared database, and it marks encrypted RCS as restricted. Messages for Web does receive them. `libgm` pairs as a **tablet**, which runs alongside your normal messages.google.com tab, so you can keep using the browser.

## How it works

- `smsbridge daemon` stays connected and stores each live message in local SQLite. It batches new rows into the Sheet after a 3-second debounce.
- At startup and then every 6 h, it lists every conversation (inbox, archive, spam). It fetches anything missed and continues paging back through history.
- Rows are deduplicated by Google message ID. A Row Key column in the Sheet guards against double appends.
- By default, a message's text, attachments, and sender name/number are erased from the local database once it's in the Sheet. Message IDs and timestamps stay, for dedup and resume. Drafts and other rows that never reach the Sheet are erased after 7 days. Conversation names and participant numbers stay, because they label new rows. Set `"keep_local_text": true` to keep a local copy of message text.
- A **Status** tab in the Sheet shows `last_success`, `last_error` and counts. A macOS notification fires on logout, or when Sheet writes keep failing.

## Setup (macOS)

You need Go 1.27+, Firefox, and the Google Cloud CLI (`gcloud`).

1. **Build:**

   ```sh
   go build -o bin/smsbridge ./cmd/smsbridge
   ```

2. **Get a Google sign-in for pairing.** Use Firefox, because Chrome's device-bound session cookies break copied cookies.

   ```sh
   /Applications/Firefox.app/Contents/MacOS/firefox \
     -profile "$HOME/Library/Application Support/sms-bridge/firefox-profile" \
     -no-remote https://messages.google.com/web/authentication
   ```

   Sign in with the Google account your phone's Google Messages uses, then quit Firefox.

3. **Pair:**

   ```sh
   ./bin/smsbridge pair
   ```

   Tap the emoji it prints on your phone. Afterwards, delete the Firefox profile folder: it holds your Google sign-in.

   To re-pair later, stop the daemon if it's running, then run `pair` again. Afterwards, start the daemon with `./scripts/install-launchd.sh` (`pair` restarts it itself if it's still loaded).

4. **Sheet access:**

   ```sh
   gcloud auth login --enable-gdrive-access
   ```

   Sign in with the Google account that should own the Sheet. Then create a Sheet in that account.

   Alternatively, put a service-account key at `~/Library/Application Support/sms-bridge/service-account.json` and share the Sheet with that account. A key there takes precedence over `gcloud`.

5. **Config.** Write `~/Library/Application Support/sms-bridge/config.json`:

   ```json
   {
     "spreadsheet_id": "<id from the Sheet URL>",
     "gcloud_account": "<the account you logged in with>"
   }
   ```

   Optional keys:
   - `gcloud_bin` (default `/opt/homebrew/bin/gcloud`).
   - `keep_local_text` (default `false`).
   - `service_account_key` (a path).

6. **Run it always-on:**

   ```sh
   ./scripts/install-launchd.sh
   ```

7. **Check it:**

   ```sh
   ./bin/smsbridge status
   ```

Other commands:
- `smsbridge sync [-backfill-pages N]`: one-off catch-up.
- `smsbridge push`: push pending rows.
- `smsbridge run`: a gated one-shot for cron-style use.

These share one session lock with the daemon. While the daemon runs, they refuse to start; it already syncs and pushes on its own.

## Sending texts

Only the daemon is connected to your phone, so sends go through it over a Unix socket (`send.sock`, mode 0600) in the data folder.

```sh
smsbridge lookup "Alex"                             # find conversations by name or number (JSON)
printf '%s' "Running 5 min late" | smsbridge send -conv ID
smsbridge send -to +15550100123 -text "hi"          # by number
smsbridge send -conv ID -attach /abs/path/pic.png -text "caption"
smsbridge send -conv ID -dry-run                    # show the recipient without sending
```

`send` prints one JSON line and its exit code tells you what happened:

| exit | meaning |
|---|---|
| 0 | `sent` |
| 1 | `failed` (definitely not sent) |
| 3 | `unknown`: the phone accepted it but hasn't confirmed. Don't retry, or it may go out twice. |

Group conversations need `-allow-group`.

RCS chats drop an image sent with a caption as one message, so for RCS the image goes first and the caption follows as its own message.

The daemon also checks recent conversations every 2 minutes. If it finds messages the live stream missed, it re-arms the phone's push subscription, and it reconnects if the gap persists.

## Caveats

- `session.json` in the data folder is equivalent to access to your Google account. Keep the folder `0700`, and consider turning on FileVault.
- The LaunchAgent only runs while your macOS user is logged in.
- Rows are append-only. Later edits and deletions on the phone aren't reflected in the Sheet.
- Google may unpair inactive devices or log the session out. Re-run `pair` when notified.

## License

AGPL-3.0, because `libgm` is AGPL-3.0. See [LICENSE](LICENSE).
