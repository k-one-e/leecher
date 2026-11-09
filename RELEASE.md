Lightweight, self-hosted downloader with a web UI: media from YouTube, Twitter/X, Instagram, TikTok, Reddit and dozens more (yt-dlp), direct file URLs, and magnet / `.torrent` links (via Transmission). Single binary, runs on your own hardware — from a Raspberry Pi up. Everything is behind a login with per-user accounts.

## Features

- **Retry failed downloads** — failed or stopped downloads with a source link can be retried from Activity, preserving the original format and destination, including after a restart. The new attempt replaces the failed row, duplicate retries are blocked, and the original stays in admin history. Older YouTube Music album and SoundCloud set links recover MP3 ZIP mode even without a saved format.
- **Accounts** — admins and users, bcrypt passwords, forced password change for new accounts, per-user API tokens for scripts, shortcuts and the bookmarklet; users only see their own downloads.
- **Three engines** — yt-dlp for supported sites, a resumable direct downloader for plain file URLs, and Transmission for magnets and `.torrent` files; leecher can be your browser's handler for `magnet:` links.
- **Formats and presets** — pick resolution/codec/size, or one click for best quality, 1080p or MP3; YouTube Music albums and playlists as a tagged MP3 ZIP.
- **SSRF guard** — direct downloads never reach private, loopback, link-local or cluster addresses, also not through redirects or DNS tricks.
- **Status pill** — public IP and country, free disk space and live download speed; optional **kill switch** stops downloads when the outbound IP is not one you allowed (e.g. the VPN dropped).
- **Save, Keep or Delete** — save a finished download to your device, keep it in the server library permanently, or let it expire.
- **Admin panel** — users, files, history, storage (usage, throughput, speed test), yt-dlp updates and all settings without a restart.
- **Security** — downloaded yt-dlp / ffmpeg are verified, the systemd service is confined, CSRF protection, CSP and Secure/HttpOnly/SameSite cookies.

## Install (Linux)

```bash
ARCH=$(uname -m); case $ARCH in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; armv7l|armv6l) ARCH=arm;; esac
curl -fsSL https://github.com/k-one-e/leecher/releases/download/{{VERSION}}/leecher-linux-${ARCH}.tar.gz | tar xz
sudo mv leecher /opt/leecher
cd /opt/leecher
sudo bash install.sh          # system user, yt-dlp + ffmpeg, systemd service
sudo systemctl start leecher
sudo -u leecher ./leecher --add-user <name> '<password>' admin
```

Open `http://<your-ip>:9090` and sign in.

## Run (macOS / Windows)

```bash
tar xzf leecher-darwin-arm64.tar.gz
cd leecher
cp config.json.example config.json
./leecher --add-user <name> '<password>' admin
./leecher
```

Full documentation: [README](https://github.com/k-one-e/leecher#readme).
