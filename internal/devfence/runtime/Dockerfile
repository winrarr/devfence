FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash build-essential ca-certificates curl gh git jq make nodejs npm \
       openssh-client python3 sudo \
    && npm install --global n \
    && n 22 \
    && npm install --global @openai/codex @anthropic-ai/claude-code \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY devfence-exec /usr/local/bin/devfence-exec
RUN chmod 0755 /usr/local/bin/devfence-exec \
    && mkdir -p /run/devfence /run/secrets

CMD ["sleep", "infinity"]
