# Claude Code fixture'ları

Tüm içerik sentetiktir; gerçek prompt, yol veya kimlik içermez.

## session-basic.jsonl

Beklenen parser çıktısı (tekilleştirme `message.id` + `requestId` ile):

| Alan | Değer |
|---|---|
| Benzersiz yanıt | 2 (`msg_A` iki satırda tekrarlanır) |
| input_tokens | 15 |
| output_tokens | 300 |
| cache_read_input_tokens | 11000 |
| cache_creation_input_tokens | 1000 |
| Kullanıcı mesajı (tool_result hariç) | 1 |
| Atlanan satır (bozuk JSON) | 1 |
| Bilinmeyen `type` (yok sayılan) | 1 (`future-event-type`) |

## session-subagent.jsonl

Alt ajan (`isSidechain: true`) satırları, `<synthetic>` model, `requestId` eksik satır ve aralık dışı token içerir. Alt ajan satırlarının ana oturumla aynı `sessionId`'yi taşıdığı **varsayımdır** (gerçek veride doğrulanmadı).

| Alan | Değer |
|---|---|
| Benzersiz yanıt | 3 (`msg_P1`, `msg_S1`, `msg_P2`); `msg_S1` iki satırda tekrarlanır (1 yinelenen) |
| `claude-sonnet-5` | input 25, output 140, cache_read 2500, cache_creation 500 (1 sa: 200), turn_count 1 |
| `claude-haiku-5` (alt ajan, ana toplama dahil) | input 3, output 50, cache_read 400, cache_creation 0, turn_count 0 |
| Alt ajan yanıtı / alt ajan kullanıcı satırı | 1 / 1 (kullanıcı turu sayılmaz) |
| Gerçek kullanıcı turu / tool_result | 1 / 2 |
| `<synthetic>` yanıt (atlanır) | 1 |
| `requestId` eksik (sayılmaz) | 1 (`msg_NOREQ`) |
| Atlanan satır | 1 (`out_of_range`, `msg_HUGE`) |
| started_at / ended_at | 2026-01-02T09:00:00Z / 2026-01-02T09:00:20Z |
| cwd / git_branch / sürüm | `/home/dev/example-project` / `feature/example` / `2.1.246` |

## session-unknown-version.jsonl

Gelecekteki bir sürümü (`9.9.9`) taklit eder: bilinmeyen alanlar, yeni satır tipi, yeni içerik bloğu tipi, biçimi bozulmuş token ve eksik `usage`.

| Alan | Değer |
|---|---|
| Benzersiz yanıt | 2 (`msg_F1`, `msg_F3`) |
| input / output / cache_read / cache_creation | 8 / 80 / 800 / 80 (1 sa: 0) |
| Gerçek kullanıcı turu | 1 (bilinmeyen blok tipli liste; tool_result değil) |
| Yok sayılan tip | 1 (`hologram-event`) |
| Atlanan satır | 2 (`bad_tokens`: `output_tokens` metin; `missing_usage`) |
