FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/inboxd ./cmd/inboxd \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent-inbox ./cmd/agent-inbox
RUN mkdir -p /out/data && chmod 0750 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/inboxd /usr/local/bin/inboxd
COPY --from=build --chown=65532:65532 /out/agent-inbox /usr/local/bin/agent-inbox
COPY --from=build --chown=65532:65532 /out/data/ /var/lib/agent-inbox/
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/inboxd"]
