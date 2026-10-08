FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /blankreel .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /blankreel /usr/local/bin/blankreel
ENV DATA_DIR=/data PORT=8080 TZ=America/New_York
EXPOSE 8080
CMD ["blankreel"]
