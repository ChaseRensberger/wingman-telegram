# Wingman Telegram

Send tasks to Wingman's Build agent from your private Telegram account.
Each task explicitly selects your configured GPT 6.1 Sol model.
The bot uses one persistent session and returns the Console link and final text reply.
Other users and group chats receive no reply.

## Setup

You need a Telegram bot token, Docker Compose, and a hosted Wingman service with the Build agent and model access.
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

Before you start the bot, enter the Wingman configuration in `.env`:

- `WINGMAN_URL`: The server origin, without `/console` or another path.
- `WINGMAN_USERNAME`: The service username. The default is `wingman`.
- `WINGMAN_PASSWORD`: The service password. Keep it outside Git.
- `WINGMAN_AGENT_ID`: The ID of the hosted Build agent, not its name.
- `WINGMAN_MODEL_REF`: The exact GPT 6.1 Sol model reference configured on the hosted server.
- `WINGMAN_WORKDIR`: An existing absolute directory on the Wingman server, not inside the Telegram container.
- `WINGMAN_CONSOLE_URL`: An optional browser-accessible origin when the API uses a private address.

Before submitting a task, the bot waits for Wingman readiness, logs the server version, and makes sure that the agent is named `Build`.
Telegram polling and saved reply delivery can start while Wingman is unavailable.
This version uses the Wingman Go SDK from release `v0.1.65`. Use the matching Wingman server release.
It sends the model reference on every task, without a fallback to another model.
Model provider credentials stay on the Wingman server.
The `identify` command needs only the Telegram token.

Start the bot:

```sh
docker compose up -d
docker compose logs -f telegram
```

Do not share `.env` or commit it to Git. It contains your bot token.
The container needs access to Telegram and the Wingman API.
Use HTTPS for remote Wingman connections or a private encrypted network such as Tailscale.
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

- `/start` and `/help` explain task submission.
- `/session` returns the Console link after the first task creates a session.
- Other slash commands return `Unknown command. Use /help.`
- Attachments receive a message that asks for text instead.

The bot forwards ordinary text unchanged to Build.
Include the public clone URL, feature instructions, and dev server requirements in your Telegram prompt.
Build can clone `https://github.com/ChaseRensberger/roast.git` without GitHub credentials.
The client does not create pull requests, expose ports, or set up the preview server.
The hosted server needs the app dependencies and a reachable preview address.
Build's final reply can include the preview link.

Tasks run one at a time. Commands and follow-up messages wait while a task runs.
The bot keeps polling Telegram and saves these messages in order, including while a task waits for approval.
Container restarts retain the saved messages.
Use Console to watch progress and approve tool actions.
Wingman permission requests time out after five minutes.
The bot shares Console's default client identity and matches replies to its own run IDs.
Stopping the bot does not cancel work that Wingman accepted.
The bot stores delivery progress in the Compose volume named `telegram-state`.
Run only one client for each bot token, and keep the stack on the same server.

Container restarts and image updates retain the volume.
The bot resumes accepted runs and retries uncertain submissions with the same request ID.
Wingman uses that ID to prevent duplicate tasks.
The bot follows session events and saves its position together with completed reply text.
After a disconnect, it reloads the session and run, then resumes the event stream if needed.
Temporary API failures retry with increasing delays and honor the server's `Retry-After` header.
Other connection or protocol failures stop the bot with its state intact. Compose restarts it.
Failed or aborted tasks return a Telegram reply.
If Wingman rejects a task submission, the bot returns the error and continues with the next saved message.
Uncertain submissions keep the same request ID across retries and restarts.
If a retry conflicts after an agent edit or session move, the bot looks for the original run before reporting rejection.
A reply can repeat if the process stops after Telegram accepts it but before the bot saves progress.
Changing the Wingman origin, agent, model, or working directory requires a separate state file or volume.
An existing canned-response state file keeps its Telegram progress and finishes any stored reply before the first task.
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
Set the Telegram and Wingman variables from `.env.example` in the stack environment.
Keep the bot token and Wingman password in Komodo's secret configuration, not in the repository.

Enable Auto Update for the stack and use the `latest` image tag.
Schedule Komodo's Global Auto Update procedure at the interval you want.
The default procedure runs daily, so change its schedule for faster updates.

A push first builds and publishes the image.
Komodo then detects the new image and replaces the container on its next update run.
Keep the volume and one active bot container across deployments.

To roll back, set `TELEGRAM_IMAGE` to an earlier `sha-<full-commit-id>` tag and redeploy.
A fixed commit tag does not receive automatic image updates.
See [Komodo's automatic update instructions](https://komo.do/docs/deploy/auto-update) for the update procedure.
