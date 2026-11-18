# Harnext agent performansı

Agent'ın geliştiricinin bilgisayarına maliyeti: tek bir makinede ölçüldü, her sayıyı tekrar üretmenin yolu var. İngilizce sürüm: [PERFORMANCE.md](PERFORMANCE.md). Agent'ın neyi okuyup neyi gönderdiği için [GIZLILIK.md](GIZLILIK.md).

## Özet

| | Sonuç |
|---|---|
| 18 transcript, 110 MB'lık ilk tarama (son 30 gün) | agent sakinleşene kadar yaklaşık 1,3 sn, 1,2 sn CPU süresi |
| Boşta | tek CPU çekirdeğinin %0,05 ile %0,17'si |
| Bellek | 17 ile 27 MB (macOS'un sürece yazdığı bellek, "footprint"); aynı süreç için `ps` 68 ile 98 MB RSS gösterir, aşağıya bakın |
| Disk | 0,1 MB (yerel veritabanı, state, status) |
| İndirme | 5,8 MB (macOS arm64), açılmış hali 14,1 MB |

Agent her dosyada son kaldığı yerden devam eder (dosya başına offset tutar); yani pahalı an ilk taramadır, sonrasında yalnızca yeni gelen satırları işler.

## Nasıl ölçüldü

Tek bilgisayar: MacBook Pro, Apple M1 Pro (10 çekirdek), macOS, arm64, Go 1.27.3. Girdi, geliştiricinin kendi Claude Code geçmişi: 18 transcript dosyası, 110,4 MB, 26.938 işlenen satır (11 oturum), son 30 gün; agent başlarken tam olarak bunu okur. Dosyalar işletim sisteminin önbelleğindeydi, yani disk hızı bu sayılara dahil değil. Linux ve Windows'ta hiçbir şey ölçülmedi.

**1. Gerçek binary** ([scripts/measure-agent.sh](../scripts/measure-agent.sh)). Agent'ı yerel bir sahte sunucuya karşı, geçici bir agent klasörüyle çalıştırır; CPU süresini ve belleği `ps` ile örnekler, ilk taramayı ve 30 ile 60 saniyelik boşta halini raporlar. Hiçbir şey yüklenmez ve gerçek agent state'ine dokunulmaz. Sahte sunucu imzalı yapılandırma vermediği için friction analizi olmadan çalışır.

| Derleme | Sakinleşene kadar soğuk başlangıç | CPU süresi | Footprint | RSS (`ps`) | Boşta CPU |
|---|---|---|---|---|---|
| Geliştirme derlemesi | ~1,3 sn | 1,17 ile 1,22 sn | 17 ile 26 MB | 68 ile 80 MB | %0,05 ile 0,13 |
| Release 0.2.3 (gizlenmiş derleme, artık dağıtılmıyor) | ~1,3 sn | 1,26 ile 1,30 sn | 21 ile 27 MB | 82 ile 100 MB | %0,10 ile 0,17 |

0.2.4'ten beri release, gizlenmemiş sade bir Go derlemesidir (agent açık kaynak): 29,2 MiB yerine 13,5 MiB. Yukarıdaki tablo bu değişiklikten önce ölçüldü ve aynı veriyle tekrarlanmadı; özetteki boyutlar yeni derleme içindir.

Aralık, her biri için 3 ile 5 çalıştırmanın en düşük ve en yüksek değeridir. İki bellek ölçüsü gösteriliyor, çünkü macOS'ta aralarında büyük fark var: RSS, Go'nun serbest bıraktığı ama macOS'un henüz geri almadığı sayfaları da sayar (macOS bunu yalnızca bellek baskısında yapar); bu yüzden footprint'ten üç dört kat yüksek okunur. Alıntılanacak sayı footprint'tir; RSS bütünlük için verilir. Footprint betiğe sonradan eklendi, bu yüzden her derlemenin son 3 çalıştırmasından gelir.

**2. Tarama, tek süreçte, friction analiziyle** (`internal/watcher` içindeki `TestMeasureScanPlain` ve `TestMeasureScanAnalyzed`). Aynı dosyaları dolaşır, her satırı parse eder ve toplar; ağ ve veritabanı yoktur.

| | Duvar saati | CPU süresi | Hız | Tepe RSS |
|---|---|---|---|---|
| Analiz olmadan | 0,49 ile 0,53 sn | 0,51 ile 0,53 sn | 207 ile 226 MB/sn | 38 ile 39 MB |
| Analizle (production varsayılan yapılandırması) | 0,78 ile 0,82 sn | 0,82 ile 0,84 sn | 135 ile 141 MB/sn | 41 ile 42 MB |

Tepe RSS, test sürecinin taramadan önce kullandığı yaklaşık 12 ile 14 MB'ı içerir. Bu, yukarıdaki footprint'ten farklı bir ölçüdür (RSS, veritabanı ve ağ olmadan tek süreçte); iki tablo birbirine eklenmek veya bire bir karşılaştırılmak için değildir.

**3. Sentetik fixture üzerinde sıcak yol.** `BenchmarkParseAndAdd` küçük bir transcript'i parse edip aggregator'a ekler: yaklaşık 29 µs, yaklaşık 120 MB/sn, geçiş başına 119 allocation.

## Tekrar üretin

```sh
make build && ./scripts/measure-agent.sh                         # gerçek binary; başka bir derleme için yol verin
go test -run '^$' -bench ParseAndAdd -benchmem ./internal/aggregate
go test -tags realdata -count=1 -run TestMeasureScanPlain -v ./internal/watcher
HARNEXT_REALDATA_CONFIG=cfg.json go test -tags realdata -count=1 -run TestMeasureScanAnalyzed -v ./internal/watcher
```

`cfg.json`, sunucunun varsayılan agent yapılandırmasının JSON'udur. Gerçek veri testlerini ayrı süreçlerde çalıştırın, çünkü tepe bellek süreç başına bir maksimumdur. Testler yalnızca boyut, süre ve sayı yazar; yol ve içerik yazmaz.

## Teknik soru-cevap

**Editörümü veya AI aracımı yavaşlatır mı?** Agent aracınızın yolunda değildir. Ağ trafiğine proxy girmez; aracınızın yazdığı log dosyalarını sonradan okur. Boştayken tek çekirdeğin yüzde 0,2'sinden azını kullanır.

**İlk başlangıç neden bir saniye CPU harcıyor?** Son 30 günün geçmişini bir kez, en yeniden eskiye doğru okur; böylece en yeni oturumlar önce yüklenir. Sonrasında yalnızca yeni satırları okur.

**Ne kadar bellek kullanır?** Ölçülen makinede, tarama sırasında ve boştayken, işletim sisteminin saydığı haliyle (footprint) yaklaşık 17 ile 27 MB. `ps` aynı süreç için 68 ile 98 MB RSS gösterir; çünkü Go'nun serbest bıraktığı ama macOS'un henüz geri almadığı belleği de sayar. Agent'ın içindeki dağılım (Go runtime'ı, gömülü SQLite, izleyiciler, toplanmış state) profil edilmedi. Linux ve Windows ölçülmedi, bellek orada farklı sayılır.

**Sunucuya ulaşılamazsa ne olur?** Yüklemeler artan bekleme süresiyle yeniden denenir; sayaçlar yerel veritabanında tutulur ve sunucu yanıt verince tekrar gönderilir. Hiçbir şey kaybolmaz ve içerik biriktirilmez, yalnızca sayılar.

**İmzalı yapılandırma yoksa?** Friction skorları boş kalır (agent sıfır değil, hiç skor göndermez). Token, maliyet ve konu verisi yine toplanır.

**Hangi sistemler destekleniyor?** macOS (arm64, amd64), Linux (amd64, arm64) ve Windows (amd64); launchd, systemd `--user` ve Task Scheduler servisleriyle. Performans yalnızca macOS arm64'te ölçüldü.

**Binary nasıl korunuyor, nasıl güncelleniyor?** Release derlemesi gizlenmemiştir; kaynak herkese açıktır (aşağıda). Release paketleri SHA-256 ile doğrulanır; güncelleme bildirimi ve agent yapılandırması Ed25519 ile imzalıdır, agent yalnızca güvendiği anahtarla imzalı, daha yüksek sürümü kabul eder. Güncelleme `HARNEXT_NO_AUTO_UPDATE=1` ile kapatılabilir.

**Agent açık kaynak mı?** Evet, Apache 2.0: github.com/harnext/harnext-agent. Harnext sunucusu ve paneli değildir.

**Ne gönderiyor, nasıl kontrol ederim?** [GIZLILIK.md](GIZLILIK.md). `harnext-agent preview`, göndermeden önce tam olarak neyin gideceğini gösterir.
