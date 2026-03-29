64-bit + arm64

```
GOOS=windows GOARCH=amd64 go build -o bin/x64_windows_goondvr.exe &&
GOOS=darwin GOARCH=amd64 go build -o bin/x64_macos_goondvr &&
GOOS=linux GOARCH=amd64 go build -o bin/x64_linux_goondvr &&
GOOS=windows GOARCH=arm64 go build -o bin/arm64_windows_goondvr.exe &&
GOOS=darwin GOARCH=arm64 go build -o bin/arm64_macos_goondvr &&
GOOS=linux GOARCH=arm64 go build -o bin/arm64_linux_goondvr
```

64-bit Windows, macOS, Linux:

```
GOOS=windows GOARCH=amd64 go build -o bin/x64_windows_goondvr.exe &&
GOOS=darwin GOARCH=amd64 go build -o bin/x64_macos_goondvr &&
GOOS=linux GOARCH=amd64 go build -o bin/x64_linux_goondvr
```

arm64 Windows, macOS, Linux:

```
GOOS=windows GOARCH=arm64 go build -o bin/arm64_windows_goondvr.exe &&
GOOS=darwin GOARCH=arm64 go build -o bin/arm64_macos_goondvr &&
GOOS=linux GOARCH=arm64 go build -o bin/arm64_linux_goondvr
```

Build Docker Tag:

```
docker build -t yamiodymel/goondvr:2.0.0 .
docker push yamiodymel/goondvr:2.0.0
docker image tag yamiodymel/goondvr:2.0.0 yamiodymel/goondvr:latest
docker push yamiodymel/goondvr:latest
```
