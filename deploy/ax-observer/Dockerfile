# AX Observer Control Room + Bug Bash Orchestrator (ADR-0005, ADR-0008, ADR-0010)
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/ax-bug-bash
COPY bin/linux_amd64/ax-observer /usr/local/bin/ax-observer
COPY services /opt/ax-bug-bash/services
COPY docs/adr /opt/ax-bug-bash/docs/adr
COPY GLOSSARY.md /opt/ax-bug-bash/GLOSSARY.md
COPY harnesses/bin /opt/ax-bug-bash/harnesses/bin
RUN chmod +x /usr/local/bin/ax-observer /opt/ax-bug-bash/harnesses/bin/*

EXPOSE 3000
ENTRYPOINT ["/usr/local/bin/ax-observer"]
