# httpreplay

`httpreplay` is a CLI tool for replaying HTTP requests from a tape file, allowing you to simulate traffic with configurable QPS (queries per second), concurrency limits, and timeouts. It supports dry-run mode for testing and logs failed requests for easy debugging.

## Features

- Replay HTTP requests from a specified tape file.
- **Tracks and saves progress for resumable operations.**
- Configurable QPS and concurrency limits.
- Adjustable HTTP request timeout.
- Dry-run mode to preview requests without sending them.
- Logs failed requests to a failure tape file.
- Detailed progress logging with success rate and QPS metrics.

## Installation

[**Download the latest binary**](https://github.com/roy2220/httpreplay/releases)

## Usage

Run `httpreplay` with the required tape file and optional flags:

```bash
httpreplay TAPE-FILE [-q QPS] [-c CONCURRENCY] [-t TIMEOUT] [-f] [-d]
```

### Arguments

- **TAPE-FILE** (required): Path to the tape file containing HTTP requests. Each line should be a valid HTTP request (e.g., `curl`-like format: `URL [-X METHOD] [-H HEADER]... [-d DATA]`).
- **-q QPS**: Queries per second limit (default: 1). Set to < 1 for no limit.
- **-c CONCURRENCY**: Concurrent requests limit (default: 1). Set to < 1 for no limit.
- **-t TIMEOUT**: HTTP request timeout in seconds (default: 10). Set to < 1 for no timeout.
- **-f**: Follow HTTP redirects (default: false).
- **-d**: Preview requests without sending them (default: false).

**Note**: At least one of QPS or concurrency must be limited (i.e., ≥ 1).

### Tape File Format

The tape file contains one HTTP request per line, in a `curl`-like format. Example:

```
https://example.com/api -X POST -H "Content-Type: application/json" -d '{"key":"value"}'
https://example.com/get -X GET -H "Authorization: Bearer token"
```

Each line is parsed into an HTTP request with method, URL, headers, and optional body.

### Example Commands

1. Replay requests with a QPS limit of 10 and concurrency of 5:
   ```bash
   httpreplay requests.txt -q 10 -c 5
   ```

2. Run in dry-run mode with a 30-second timeout:
   ```bash
   httpreplay requests.txt -t 30 -d
   ```

3. Replay without QPS or concurrency limits:
   ```bash
   httpreplay requests.txt -q 0 -c 0
   ```

### Output Files

- **Failure Tape File** (`TAPE-FILE.httpreplay-failure`): Stores failed requests for retry or analysis.
- **Position File** (`TAPE-FILE.httpreplay-pos`): Tracks the last processed request index for resuming.
- **Dry-run Position File** (`TAPE-FILE.httpreplay-pos.dry-run`): Used in dry-run mode to avoid overwriting the main position file.

### Logging

- **Progress Logs**: Displayed every second, showing tape position, QPS, concurrency, successful/failed requests, and success rate.
- **Debug Logs**: Enable with `DEBUG=1` environment variable to log parsed HTTP requests.
- **Errors/Warnings**: Issues like file read errors or parsing failures are logged with `[WARN]` or `[FATAL]`.

Example progress log:
```
[INFO] current progress: tapePosition=100 qps=50 concurrency=5 successful=95 failed=5 successRate=0.95
```

## Quick Start

1. Create a tape file (`requests.txt`) with HTTP requests:
   ```
   https://example.com/api -X GET
   https://example.com/api -X POST -d '{"data":"test"}'
   ```

2. Run the tool:
   ```bash
   httpreplay requests.txt -q 5 -c 2 -t 15
   ```

3. Check logs and the failure tape file (`requests.txt.httpreplay-failure`) for any failed requests.

## Notes

- The tool resumes from the last processed request using the position file.
- Failed requests are appended to the failure tape file every 500ms.
- Progress is saved every 200ms to ensure resumability.
- Use `Ctrl+C` or `SIGTERM` to gracefully stop the tool, ensuring files are closed properly.

## Troubleshooting

- **Invalid tape file format**: Ensure each line follows the `curl`-like syntax. Check logs for parsing errors.
- **High failure rate**: Verify the target server is reachable and check the failure tape file for details.
- **Resuming issues**: Ensure the position file is not corrupted or manually deleted.
- **Performance issues**: Adjust QPS and concurrency limits to balance load and stability.

## License

MIT License. See `LICENSE` for details.
