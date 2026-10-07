# Satisfactory Server Dashboard

A lightweight, read-only web dashboard written in Go that runs alongside the Satisfactory dedicated game server container.

![Dashboard Preview](web/index.html)

## Features

- **Resource Utilization Graphs**: Real-time dual-line graphs tracking CPU (%) and Memory (GB / % of limit) over time, with reference lines for memory reservation (4GB) and memory limit (8GB).
- **Log Parsing & Event Stream**: Continuously tails `/config/gamefiles/FactoryGame/Saved/Logs/FactoryGame.log` to detect:
  - Pioneer join & disconnect events
  - World saves and autosaves (with save duration)
  - Unreal Engine startup, SteamCMD updates, and Server API readiness
  - Server crash or session warnings
- **Satisfactory 1.0 Dedicated Server API Integration**:
  - Periodically checks server health via `https://satisfactory-server:7777/api/v1` (HealthCheck)
  - Supports authenticated `QueryServerState` if `API_TOKEN` is provided
- **Save File & Backup Inspector**: Lists `.sav` files and backup archives found on disk with file size and modified timestamps.
- **Ultra Lightweight**: Single static Go binary inside an Alpine container. Uses ~10–15 MB RAM with zero external runtime dependencies.

## Architecture & Integration

```
                         Docker Network
                  +---------------------------+
                  |                           |
                  |   satisfactory-server     |
                  |     (Game Server)         |
                  |                           |
                  |   Ports: 7777 UDP/TCP     |
                  +-------------+-------------+
                                |
             +------------------+------------------+
             |                                     |
    Shared Volume (:ro)                  HTTPS HealthCheck (:7777)
             |                                     |
             v                                     v
+-------------------------------------------------------------+
|                                                             |
|                   satisfactory-dashboard                    |
|                (Go Web Container :8080)                     |
|                                                             |
|   - Realtime CPU/RAM Metrics Collector                      |
|   - FactoryGame.log Event Parser                            |
|   - Embedded FICSIT Industrial Web UI                       |
|                                                             |
+-------------------------------------------------------------+
                                |
                         Web Browser (:8080)
```

## Running with Docker Compose

Add the service to your `docker-compose.yml`:

```yaml
services:
  satisfactory-server:
    container_name: 'satisfactory-server'
    image: 'robtme/satisfactory-server:latest'
    ports:
      - '7777:7777/tcp'
      - '7777:7777/udp'
      - '8888:8888/tcp'
    volumes:
      - './satisfactory-server:/config'
    ...

  satisfactory-dashboard:
    container_name: 'satisfactory-dashboard'
    build:
      context: ./dashboard
    ports:
      - '8080:8080'
    volumes:
      - './satisfactory-server:/config:ro'
      - '/var/run/docker.sock:/var/run/docker.sock:ro'
    environment:
      - PORT=8080
      - SERVER_API_URL=https://satisfactory-server:7777
      - TARGET_CONTAINER=satisfactory-server
      - POLL_INTERVAL=5
    depends_on:
      - satisfactory-server
    restart: unless-stopped
```

Start the containers:

```bash
docker compose up -d --build
```

Then open `http://<your-host-ip>:8080` in your web browser.

## Configuration Options

| Environment Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | Port the web dashboard listens on |
| `SERVER_API_URL` | `https://satisfactory-server:7777` | Internal HTTPS URL to the game server API |
| `TARGET_CONTAINER` | `satisfactory-server` | Container name for reading Docker stats via `/var/run/docker.sock` |
| `POLL_INTERVAL` | `5` | Metric sampling interval in seconds |
| `API_TOKEN` | _(empty)_ | Optional Dedicated Server API Token for extended session information |
| `LOG_FILE_PATH` | _(empty)_ | Override path to the server log file if located outside standard paths |

