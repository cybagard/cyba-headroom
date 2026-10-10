# Security

## Reporting a vulnerability

Report a vulnerability in private through GitHub. On the repository page, open **Security**, then **Report a vulnerability**. Do not open a public issue for a vulnerability.

## What headroom contacts and stores

headroom reads the `/api/ps` endpoint of Ollama only while an `ollama serve` process runs. It never contacts a host that is not this Mac. A host is this Mac when it is a loopback address or `localhost`.

The daemon writes the sample files with mode 0600. The samples hold these data:

- memory figures;
- the name and path of each worktree;
- container names and images;
- bind-mount paths;
- Tart VM names and shared directories;
- model names.

The samples never hold container labels, environment or command lines.
