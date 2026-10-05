---
title: Self-hosting models
nav_order: 5
---

# Self-hosting models
{: .no_toc }

dunce-union is an optional kit for running the models yourself. If you already host models, skip this. Ignatius only needs a URL and auth.

It is a compose setup in `deploy/dunce-union/`. Each model is one service: an unmodified Decis engine image with the weights baked in, running behind the gateway.

1. Copy the example env file, then set both keys.

```sh
cd deploy/dunce-union
cp .env.example .env
```

2. Set `COMPOSE_PROFILES` in `.env` to pick which models run. Each engine holds several GB of memory, so start only the ones you need. Compressed image sizes: laya 4.4G, jeff 5.9G, kev 6.0G, jeff-gemma 17.7G.
3. Start them.

```sh
docker compose up -d --wait
```

4. Talk to the gateway on port 8081. It is the only published port. The model services are reachable only on the compose network.

Models vary in quality, and so does confidence calibration. Test a cascade on your own data before you rely on it. See [Judging routes](eval.html).
