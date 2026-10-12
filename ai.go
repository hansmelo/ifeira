package main

// Reading receipts and product photos with Claude. The API key stays on the
// server; the family only needs to be logged in.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

const maxAIUpload = 6 << 20

type aiError struct {
	status int
	code   string
}

func (e *aiError) Error() string { return e.code }

// POST /api/ai/receipt  (multipart: "image" file or "text", plus "today" YYYY-MM-DD)
func (a *App) handleReceipt(w http.ResponseWriter, r *http.Request) {
	if a.cfg.AnthropicKey == "" {
		writeError(w, http.StatusServiceUnavailable, "ai_disabled")
		return
	}
	img, mime, text, today, ok := readAIForm(w, r)
	if !ok {
		return
	}
	if img == nil && strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadRequest, "empty")
		return
	}
	if len(text) > 20000 {
		text = text[:20000]
	}
	out, err := a.askClaude(r.Context(), receiptPrompt(text, img != nil, today), img, mime)
	if err != nil {
		aiFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/ai/identify  (multipart: "image" file, plus "today")
func (a *App) handleIdentify(w http.ResponseWriter, r *http.Request) {
	if a.cfg.AnthropicKey == "" {
		writeError(w, http.StatusServiceUnavailable, "ai_disabled")
		return
	}
	img, mime, _, today, ok := readAIForm(w, r)
	if !ok {
		return
	}
	if img == nil {
		writeError(w, http.StatusBadRequest, "empty")
		return
	}
	out, err := a.askClaude(r.Context(), identifyPrompt(today), img, mime)
	if err != nil {
		aiFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func readAIForm(w http.ResponseWriter, r *http.Request) (img []byte, mime, text, today string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAIUpload)
	if err := r.ParseMultipartForm(maxAIUpload); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large")
		return nil, "", "", "", false
	}
	today = r.FormValue("today")
	if !datePattern.MatchString(today) {
		today = time.Now().UTC().Format("2006-01-02")
	}
	text = r.FormValue("text")
	if f, _, err := r.FormFile("image"); err == nil {
		defer f.Close()
		img, err = io.ReadAll(f)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_image")
			return nil, "", "", "", false
		}
		mime = http.DetectContentType(img)
		switch mime {
		case "image/jpeg", "image/png", "image/webp", "image/gif":
		default:
			writeError(w, http.StatusUnsupportedMediaType, "not_an_image")
			return nil, "", "", "", false
		}
	}
	return img, mime, text, today, true
}

// askClaude sends the prompt (and image) and returns the JSON object Claude wrote.
func (a *App) askClaude(ctx context.Context, prompt string, img []byte, mime string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 110*time.Second)
	defer cancel()

	var content []map[string]any
	if img != nil {
		content = append(content, map[string]any{
			"type":   "image",
			"source": map[string]string{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(img)},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt + "\nResponda só com o JSON, sem nenhum outro texto."})
	body, _ := json.Marshal(map[string]any{
		"model":      a.cfg.AnthropicModel,
		"max_tokens": 4096,
		"messages":   []map[string]any{{"role": "user", "content": content}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.AnthropicURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.cfg.AnthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &aiError{http.StatusBadGateway, "ai_unreachable"}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		log.Printf("claude: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 400)])))
		switch {
		case res.StatusCode == 401 || res.StatusCode == 403:
			return nil, &aiError{http.StatusBadGateway, "ai_auth"}
		case res.StatusCode == 429:
			return nil, &aiError{http.StatusTooManyRequests, "ai_rate"}
		case res.StatusCode == 400 && bytes.Contains(bytes.ToLower(raw), []byte("credit")):
			return nil, &aiError{http.StatusPaymentRequired, "ai_credit"}
		case res.StatusCode == 404:
			return nil, &aiError{http.StatusBadGateway, "ai_model"}
		}
		return nil, &aiError{http.StatusBadGateway, "ai_error"}
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, &aiError{http.StatusBadGateway, "ai_error"}
	}
	var sb strings.Builder
	for _, c := range msg.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	obj, err := looseJSON(sb.String())
	if err != nil {
		return nil, &aiError{http.StatusBadGateway, "ai_bad_json"}
	}
	return obj, nil
}

var fencePattern = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// looseJSON accepts a reply that is JSON, JSON inside a code fence, or JSON with a sentence around it.
func looseJSON(t string) (json.RawMessage, error) {
	t = strings.TrimSpace(t)
	try := func(s string) (json.RawMessage, bool) {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "{") && json.Valid([]byte(s)) {
			return json.RawMessage(s), true
		}
		return nil, false
	}
	if v, ok := try(t); ok {
		return v, nil
	}
	if m := fencePattern.FindStringSubmatch(t); m != nil {
		if v, ok := try(m[1]); ok {
			return v, nil
		}
	}
	if i, j := strings.Index(t, "{"), strings.LastIndex(t, "}"); i >= 0 && j > i {
		if v, ok := try(t[i : j+1]); ok {
			return v, nil
		}
	}
	return nil, errors.New("no json")
}

func aiFail(w http.ResponseWriter, err error) {
	var ae *aiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, http.StatusGatewayTimeout, "ai_timeout")
		return
	}
	log.Printf("claude: %v", err)
	writeError(w, http.StatusBadGateway, "ai_error")
}

func receiptPrompt(text string, hasImage bool, today string) string {
	src := " (foto anexada)"
	if !hasImage {
		src = " (o texto está abaixo)"
	}
	p := fmt.Sprintf(`Leia esta nota fiscal de supermercado%s e liste todos os produtos comprados.
Regras:
- Ignore totais, subtotais, impostos, forma de pagamento, troco, pontos de fidelidade, dados da loja e linhas de desconto.
- Transforme os nomes abreviados da nota em nomes claros, como alguém escreveria numa lista de compras, em português do Brasil. Exemplo: "LTE UHT INT ITALAC 1L" -> "Leite integral Italac 1L".
- Se o mesmo produto aparecer em várias linhas, junte e some as quantidades.
- qty = número de unidades compradas. Para itens vendidos por peso, qty = 1 e weight_g = o peso em gramas.
- Quando o tamanho da embalagem estiver impresso (1L, 500g, 2kg), coloque em weight_g em gramas (ml para líquidos). Senão, null.
- category: uma de dairy, eggs, meat, fish, veg, fruit, drinks, sauces, frozen, bakery, leftovers, snacks, pantry, household, other.
- storage: onde guardar em casa: freezer (congelador), shelf (prateleira da geladeira), drawer (gaveta da geladeira), door (porta da geladeira), pantry (armário, sem refrigeração). Produtos de limpeza e higiene vão para pantry.
- shelf_life_days: número típico de dias até precisar consumir, guardado desse jeito.
- date: a data da compra impressa na nota, como AAAA-MM-DD, ou null.
Hoje é %s.
Formato:
{"store":"Mercado Exemplo","date":"2026-10-10","items":[{"name":"Leite integral 1L","qty":2,"weight_g":1000,"category":"dairy","storage":"door","shelf_life_days":7,"price":5.49}]}`, src, today)
	if !hasImage {
		p += "\n\nTexto da nota:\n" + text
	}
	return p
}

func identifyPrompt(today string) string {
	return fmt.Sprintf(`Esta é a foto de um produto de supermercado ou de casa. Identifique-o.
Responda com JSON neste formato:
{"name":"Iogurte natural Nestlé 170g","category":"dairy","weight":170,"weight_unit":"g","expiry":"2026-10-20","expiry_printed":true,"shelf_life_days":10}
- name: nome curto e claro em português do Brasil, com marca se aparecer.
- category: uma de dairy, eggs, meat, fish, veg, fruit, drinks, sauces, frozen, bakery, leftovers, snacks, pantry, household, other.
- weight e weight_unit (g, kg, ml ou L): o peso ou volume impresso na embalagem, ou null.
- expiry: a data de validade impressa (VAL, validade, best before) como AAAA-MM-DD, ou null se não estiver legível. Não invente.
- expiry_printed: true se a data foi lida da embalagem.
- shelf_life_days: dias típicos até precisar consumir, guardado do jeito certo.
Hoje é %s.`, today)
}
