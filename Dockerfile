# The API server and the admin tool, in one small image.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wearwhat ./cmd/wearwhat

FROM alpine:3.22
# Render's SSH needs the running user to have a shell, an unlocked account and a ~/.ssh folder
# set to 0700. `adduser -D` locks the account, and the `*` password unlocks it without allowing
# any password login.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -s /bin/sh wearwhat \
 && echo 'wearwhat:*' | chpasswd -e \
 && install -d -m 0700 -o wearwhat -g wearwhat /home/wearwhat/.ssh
COPY --from=build /out/ /app/
WORKDIR /app
USER wearwhat
EXPOSE 8080
CMD ["/app/server"]
