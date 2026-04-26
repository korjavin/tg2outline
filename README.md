# tg2outline

A Telegram bot that turns each message you send into a new document in your self-hosted [Outline](https://www.getoutline.com/) inbox collection. Photos are uploaded as native Outline attachments and embedded inline.

Forked from [tg2dynalist](https://github.com/korjavin/tg2dynalist).

## Features

- Each Telegram message becomes a new published Outline document
- Photos uploaded via Outline's native attachments API and embedded as inline images
- Captions and forward attribution are preserved
- Authenticates a single user by Telegram user ID
- Single static binary, deployable as a small Docker image

## Prerequisites

- A self-hosted Outline instance you control
- An Outline API token (Outline → Settings → API Tokens)
- The collection ID where new documents should be created (open the collection in Outline; the ID is in the URL)
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- Your Telegram user ID, e.g. from [@userinfobot](https://t.me/userinfobot)

## Environment Variables

| Name | Required | Description |
| --- | --- | --- |
| `BOT_TOKEN` | yes | Telegram bot token from BotFather |
| `TG_USER_ID` | yes | Your numeric Telegram user ID — only this user is accepted |
| `OUTLINE_URL` | yes | Base URL of your Outline instance, e.g. `https://outline.example.com` |
| `OUTLINE_API_TOKEN` | yes | Outline API token (Bearer credential) |
| `OUTLINE_COLLECTION_ID` | yes | UUID of the collection that acts as your inbox |

## Running locally

```bash
export BOT_TOKEN=your_telegram_bot_token
export TG_USER_ID=your_telegram_user_id
export OUTLINE_URL=https://outline.example.com
export OUTLINE_API_TOKEN=your_outline_api_token
export OUTLINE_COLLECTION_ID=00000000-0000-0000-0000-000000000000

go run .
```

## Docker

```bash
docker build -t tg2outline .

docker run --rm \
  -e BOT_TOKEN=... \
  -e TG_USER_ID=... \
  -e OUTLINE_URL=https://outline.example.com \
  -e OUTLINE_API_TOKEN=... \
  -e OUTLINE_COLLECTION_ID=... \
  ghcr.io/korjavin/tg2outline:latest
```

## Portainer / GitOps deployment

The repo ships a GitOps pipeline:

- Push to `master` → CI builds the image and tags it with the commit SHA (no `:latest`).
- CI then force-pushes a `deploy` branch whose `docker-compose.yml` has the image pinned to that SHA.
- A `PORTAINER_REDEPLOY_HOOK` repo secret (Portainer stack webhook URL) triggers the redeploy.

Portainer stack config:

- **Repository:** `https://github.com/korjavin/tg2outline`
- **Reference:** `refs/heads/deploy` (NOT `master`)
- **Compose path:** `docker-compose.yml`
- **Environment variables on the stack:** `BOT_TOKEN`, `TG_USER_ID`, `OUTLINE_URL`, `OUTLINE_API_TOKEN`, `OUTLINE_COLLECTION_ID`
- **Webhook:** enable, copy the URL into the `PORTAINER_REDEPLOY_HOOK` GitHub secret at https://github.com/korjavin/tg2outline/settings/secrets/actions

Each deployed container is traceable to an exact commit via the SHA tag.

## How it works

1. The bot polls Telegram for new messages from the authorized user.
2. For each message, it builds a markdown body containing:
   - a quoted "Forwarded from …" line if the message is a forward,
   - the message text and/or caption,
   - an embedded `![](attachment-url)` for photos.
3. It calls `POST /api/attachments.create` (if there's a photo) to obtain a presigned upload URL, then `POST`s the bytes to that URL.
4. It calls `POST /api/documents.create` with `publish: true` and `collectionId` set, creating a new document. The first line of text becomes the title (capped at 80 chars); empty messages with only a photo get the title `Telegram photo`.

## License

MIT
