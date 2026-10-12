# iFeira (Go + Vercel + Turso)

Controle da geladeira e despensa para a família toda: monte as prateleiras, adicione produtos à mão, por foto ou escaneando a nota fiscal, e arraste cada item para o lugar. Todos entram com a mesma senha e veem a mesma geladeira.

- **Go** (only the standard library, no dependencies) serves the app and the API.
- **Turso** (a hosted SQLite database, free plan) keeps products, shelves and photos.
- **Vercel** (free Hobby plan) runs the Go server and gives you HTTPS and the domain.
- **Claude** (optional) reads receipts and product photos. Without it, receipts are read by a free text reader on the phone.

## What's in the folder

| Path | What it does |
|---|---|
| `main.go` | Starts the server, routes, configuration |
| `auth.go` | Family password and the login cookie |
| `db.go` | Small Turso client over HTTPS |
| `store.go` | Products, shelves and photos API |
| `ai.go` | Receipt and product-photo reading with Claude |
| `web/` | The app (page, icons, offline support), built into the program |
| `vercel.json` | Tells Vercel this is a Go app |

## Deploy, step by step (about 15 minutes)

### 1. Create the database on Turso
1. Sign up at https://turso.tech (free).
2. Create a database, for example `ifeira`. Pick the region closest to you (São Paulo if offered).
3. Copy two values from the database page:
   - the **URL**, which looks like `libsql://ifeira-yourname.turso.io`
   - a new **token** (create one with read and write access)

You don't need to create tables: the app creates them on first use.

### 2. Put the code on GitHub
1. Create a free account at https://github.com and a new **private** repository called `ifeira`.
2. Upload every file and folder from this project (keep the `web/` folder), then commit.

### 3. Deploy on Vercel
1. Sign up at https://vercel.com with your GitHub account (free Hobby plan).
2. Click **Add New → Project** and import the `ifeira` repository. Vercel detects Go from `vercel.json`.
3. Before deploying, open **Environment Variables** and add:

| Name | Value | Required |
|---|---|---|
| `TURSO_DATABASE_URL` | the `libsql://…` URL from step 1 | yes |
| `TURSO_AUTH_TOKEN` | the token from step 1 | yes |
| `FAMILY_PASSWORD` | the password your family will type | yes |
| `SESSION_SECRET` | any long random text | recommended |
| `ANTHROPIC_API_KEY` | your key from https://console.anthropic.com | optional, turns on reading with Claude |
| `ANTHROPIC_MODEL` | defaults to `claude-sonnet-5-5`; `claude-haiku-5-5` is cheaper | optional |

4. Click **Deploy**. In a minute the app is live at `https://ifeira-yourname.vercel.app`.
5. Open it, type the family password, and you'll see example items. Use **Limpar exemplos** when you're ready.

Every push to GitHub deploys a new version automatically. If you change an environment variable, redeploy so it takes effect.

### 4. Use your own domain (optional)
1. Register `ifeira.com.br` at https://registro.br (about R$ 40 a year, needs a CPF).
2. In Vercel, open the project → **Settings → Domains**, add `ifeira.com.br`, and Vercel shows the DNS record to create.
3. Create that record where your domain's DNS is managed (Registro.br or Cloudflare). HTTPS is set up automatically.

### 5. Install on the phones
Open the link in Chrome (Android) or Safari (iPhone):
- **Android:** menu **⋮ → Instalar app** (or **Adicionar à tela inicial**).
- **iPhone:** **Compartilhar → Adicionar à Tela de Início**.

## How it works

- **Shared data.** Each product is saved as its own row, so two people editing different products never overwrite each other. Open screens check for changes every 15 seconds and when the app comes back to the foreground.
- **Photos** are shrunk on the phone (about 100 KB each) and stored in the database.
- **Login** is one family password. The login lasts 180 days per device. Changing `FAMILY_PASSWORD` (without a `SESSION_SECRET`) signs everyone out.
- **Backup:** **Ajustes → Exportar backup** downloads everything, photos included, in one file. **Importar backup** restores it.
- **Offline:** the app opens without internet and shows the last copy. Changes made offline are sent when the connection returns.

## Costs
- Vercel Hobby, Turso and GitHub: free for a family. Vercel's Hobby plan is for personal, non-commercial use.
- Claude: each receipt or photo reading is charged to your Anthropic API account (usually a few cents or less). Leave `ANTHROPIC_API_KEY` empty to use only the free reader.
- Domain: about R$ 40 a year if you want `ifeira.com.br`.

## Run it on your computer
1. Install Go 1.22+ and the Turso CLI (https://docs.turso.tech/cli/installation).
2. Start a local database: `turso dev` (it prints a local address such as `http://127.0.0.1:8080`).
3. In another terminal, from this folder:

```sh
TURSO_DATABASE_URL=http://127.0.0.1:8080 FAMILY_PASSWORD=teste go run .
```

4. Open http://localhost:3000.

## Later: moving to your own server
The program runs anywhere Go runs: build it with `go build` and run it with the same environment variables (it listens on `PORT`, default 3000), for example on an Oracle Cloud free server behind Caddy for HTTPS.
