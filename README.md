# httpreplay

[![Coverage](./.badges/coverage.svg)](#)

`httpreplay` is a **high-performance, resumable** command-line interface (CLI) tool for replaying HTTP requests from a tape file. It efficiently simulates traffic while enforcing configurable **QPS** (queries per second) and **concurrency** limits.

## Features

- **Resumable Operations**: Automatically tracks and saves the last processed request position using an **atomic position file (mmap-based)**, allowing for safe, interruption-tolerant restarts (e.g., after `Ctrl+C` or system failure).
- Replay HTTP requests from a specified tape file (in a `curl`-like format).
- Configurable **QPS** and **concurrency** limits.
- Adjustable HTTP request timeout.
- Dry-run mode to preview requests without sending them.
- Logs failed requests to a dedicated failure tape file for easy retry or analysis.
- Detailed progress logging with real-time success rate and QPS metrics.
- **Graceful Shutdown**: Handles `SIGINT` and `SIGTERM` to ensure all state and logs are properly saved.

---

## Installation

[**Download the latest binary**](https://github.com/roy2220/httpreplay/releases)

---

## Usage

Run `httpreplay` with the required tape file and optional flags:

```bash
httpreplay TAPE-FILE [-q QPS] [-c CONCURRENCY] [-t TIMEOUT] [-f] [-d]
````

### Arguments

| Argument | Description | Default |
| :--- | :--- | :--- |
| **TAPE-FILE** (required) | Path to the tape file containing HTTP requests. | |
| **-q QPS** | Queries per second limit. Set to `< 1` for no limit. | `1` |
| **-c CONCURRENCY** | Concurrent requests limit. Set to `< 1` for no limit. | `1` |
| **-t TIMEOUT** | HTTP request timeout in seconds. Set to `< 1` for no timeout. | `10` |
| **-f** | Follow HTTP redirects. | `false` |
| **-d** | **Dry-run mode**: Preview requests without sending them. | `false` |

> **Note**: At least one of QPS or concurrency must be limited (i.e., $\ge 1$).

-----

## Tape File Format

The tape file contains **one HTTP request per line**, in a format similar to `curl` arguments. `httpreplay` uses a robust parser to interpret the line into an HTTP request with method, URL, headers, and optional body.

**Key Flags Supported:**

- `-X`, `--request`: HTTP method (e.g., `POST`, `PUT`). Default is `GET`.
- `-H`, `--header`: Header (e.g., `"Content-Type: application/json"`).
- `-d`, `--data`: Request body/data.

### Example Tape File (`requests.txt`)

```
https://example.com/api/status
https://example.com/api/post -X POST -H "Content-Type: application/json" -d '{"key":"value"}'
https://example.com/api/auth -X GET -H "Authorization: Bearer token"
```

-----

## Quick Start & Examples

1. **Standard replay:** Sets a **QPS limit of 10** and a **concurrency limit of 5**.

    ```
    httpreplay requests.txt -q 10 -c 5
    ```

2. **Maximum QPS:** Limits only **concurrency to 100** for maximum possible throughput (limited only by the target server and network).

    ```
    httpreplay requests.txt -q 0 -c 100
    ```

3. **Limit by QPS only:** Limits only **QPS to 50**, allowing unlimited concurrent requests.

    ```
    httpreplay requests.txt -q 50 -c 0
    ```

4. **Dry-run:** Previews requests to the console without actually sending them.

    ```
    httpreplay requests.txt -d
    ```

5. **Follow redirects:** Replays requests, following any HTTP redirects (e.g., `301` or `302`).

    ```
    httpreplay requests.txt -f
    ```

> **Tip**: You can stop the process safely with **`Ctrl+C`**. It will save its progress and can resume when run again.

### Output Files

`httpreplay` creates companion files next to your `TAPE-FILE` to manage state:

- **Failure Tape File** (`TAPE-FILE.httpreplay-failure`): Stores the raw lines of any failed requests for later analysis or retry. *Flushed every 500ms.*
- **Position File** (`TAPE-FILE.httpreplay-pos`): Tracks the last processed request index for resuming. *Uses memory-mapping for atomic, resilient updates.*
- **Dry-run Position File** (`TAPE-FILE.httpreplay-pos.dry-run`): A separate position file is used when running in dry-run mode (`-d`) to prevent overwriting the main position file.

-----

## Logging

- **Progress Logs**: Displayed every second, showing the tape position, real-time QPS, current concurrency, successful/failed counts, and success rate.
- **Debug Logs**: Enable verbose logging for parsed HTTP requests by setting the environment variable `DEBUG=1`.
- **Errors/Warnings**: Logged with `[WARN]` or `[FATAL]` for file issues, parsing failures, etc.

**Example Progress Log:**

```
[INFO] current progress: tapePosition=100 qps=50 concurrency=5 successful=95 failed=5 successRate=0.95
```

-----

## Troubleshooting

- **Invalid Tape Format**: Ensure each line strictly adheres to the `curl`-like syntax. Check logs for parsing errors.
- **Resuming Issues**: If the position file (`*.httpreplay-pos`) is manually edited or corrupted, delete it to restart the replay from the beginning.
- **Performance**: If your failure rate is high or QPS is low, adjust the `-q` (QPS limit) and `-c` (concurrency limit) flags to balance the load on the target system.

-----

## License

MIT License. See `LICENSE` for details.
