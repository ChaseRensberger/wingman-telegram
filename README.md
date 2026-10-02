# Wingman Telegram

**DONT USE THIS VIBECODED POS**

This bot replies `Request recieved.` to text messages from your private Telegram account.
It does not connect to Wingman or need Wingman credentials.
Other users and group chats receive no reply.

## Setup

You need a Telegram bot token and Docker Compose.
For a published image, use `ghcr.io/<owner>/<repository>:latest` with lowercase names.
For a local build, use the commands in the next section.

1. Copy `.env.example` to `.env`.
2. Restrict access with `chmod 600 .env`.
3. Enter your bot token and image name in `.env`.
4. Send `/start` to your Telegram bot.
5. Find your user ID before you start the bot:

```sh
docker compose run --rm --no-deps telegram identify
```

The command lists accounts with pending private messages.
Select your own account and enter its numeric ID as `TELEGRAM_USER_ID` in `.env`.
Do not run `identify` while another client uses the same bot token.

Start the bot:

```sh
docker compose up -d
docker compose logs -f telegram
```

Do not share `.env` or commit it to Git. It contains your bot token.
The container needs outgoing HTTPS access to Telegram.
It does not need an incoming port or a public webhook.

## Local build

Use this path before the first image reaches GHCR.
The local build needs Docker but does not need the Wingman source.
Copy `.env.example` to `.env` and enter your token before these commands.

Find your user ID:

```sh
docker compose -f compose.yaml -f compose.build.yaml build
docker compose -f compose.yaml -f compose.build.yaml run --rm --no-deps telegram identify
```

Enter your numeric ID in `.env`, then start the bot:

```sh
docker compose -f compose.yaml -f compose.build.yaml up -d --build
```

## Commands and state

- `/start` and `/help` explain the canned response.
- Other slash commands, including `/session`, return `Unknown command. Use /help.`
- Attachments receive a message that asks for text instead.

Ordinary text receives exactly `Request recieved.`
The bot stores delivery progress in the Compose volume named `telegram-state`.
Run only one client for each bot token, and keep the stack on the same server.

Container restarts and image updates retain the volume.
A reply can repeat if the process stops after Telegram accepts it but before the bot saves progress.
Do not run `docker compose down -v`. It deletes delivery progress and can repeat replies.

## GHCR publishing

Push this project to a GitHub repository whose default branch is `main`.
The included GitHub Actions workflow runs tests before it publishes the image.
Pull requests run tests and build the container without publishing it.

Successful pushes to `main` publish these tags:

- `ghcr.io/<owner>/<repository>:latest` for automatic updates.
- `ghcr.io/<owner>/<repository>:sha-<full-commit-id>` for a fixed version.
- Both tags support Linux AMD64 and ARM64.

The workflow uses GitHub's supplied `GITHUB_TOKEN` to publish.
No Telegram token belongs in GitHub Actions.
For private images, the deployment server needs GHCR credentials with permission to read the package.

## Komodo

Create a Komodo Stack that uses this repository's `compose.yaml`.
Set `TELEGRAM_IMAGE`, `TELEGRAM_BOT_TOKEN`, and `TELEGRAM_USER_ID` in the stack environment.
Keep the token in Komodo's secret configuration, not in the repository.

Enable Auto Update for the stack and use the `latest` image tag.
Schedule Komodo's Global Auto Update procedure at the interval you want.
The default procedure runs daily, so change its schedule for faster updates.

A push first builds and publishes the image.
Komodo then detects the new image and replaces the container on its next update run.
Keep the volume and one active bot container across deployments.

To roll back, set `TELEGRAM_IMAGE` to an earlier `sha-<full-commit-id>` tag and redeploy.
A fixed commit tag does not receive automatic image updates.
See [Komodo's automatic update instructions](https://komo.do/docs/deploy/auto-update) for the update procedure.
