FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ARG DEVFENCE_CODEX=true
ARG DEVFENCE_CLAUDE=true
ARG DEVFENCE_GH=true
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash build-essential ca-certificates curl git jq make nodejs npm \
       openssh-client python3 sudo \
    && npm install --global n \
    && n 22 \
    && if [ "$DEVFENCE_GH" = true ]; then apt-get install -y --no-install-recommends gh; fi \
    && if [ "$DEVFENCE_CODEX" = true ]; then npm install --global @openai/codex; fi \
    && if [ "$DEVFENCE_CLAUDE" = true ]; then npm install --global @anthropic-ai/claude-code; fi \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY devfence-exec /usr/local/bin/devfence-exec
RUN chmod 0755 /usr/local/bin/devfence-exec \
    && mkdir -p /run/devfence /run/secrets

CMD ["sleep", "infinity"]
