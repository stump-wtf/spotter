FROM golang:1.27.2 AS builder

# Build-time version string (git tag > branch > SHA), passed in by CI
ARG VERSION=dev

# Install Node.js and make
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && \
    apt-get install -y nodejs make

WORKDIR /app

# Copy dependency files first for better caching
COPY go.mod go.sum package.json package-lock.json* Makefile ./

# Install dependencies
RUN make docker-deps

# Copy source
COPY . .

# Flatten directory structure if nested 'spotter' folder exists
# This handles the case where source files were generated into a subdirectory
RUN if [ -d "spotter" ]; then cp -r spotter/* . && rm -rf spotter; fi

# Generate code (Ent schemas and Templ templates)
RUN make generate

# Build CSS
RUN make css

# Build binary with version injected
RUN make build-binary VERSION=${VERSION}

# Runtime Stage
FROM debian:bookworm-slim

WORKDIR /app

# Install certificates, and refresh perl-base (Essential, so already present) to the
# current bookworm-security release — the Trivy scan gates on fixed CRITICAL/HIGH CVEs.
RUN apt-get update && apt-get install -y ca-certificates perl-base && rm -rf /var/lib/apt/lists/*

# Copy artifacts
COPY --from=builder /app/spotter-server .
COPY --from=builder /app/static ./static

EXPOSE 8080

CMD ["./spotter-server"]
