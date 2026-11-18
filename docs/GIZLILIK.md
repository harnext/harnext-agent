# Harnext gizlilik: agent neyi okur, neyi gönderir

Bu sayfa, geliştiricinin bilgisayarında çalışan Harnext agent'ını anlatır. Buradaki her iddia kodla veya bir komutla doğrulanabilir; doğrulama yolları en sonda. İngilizce sürüm: [PRIVACY.md](PRIVACY.md). Hukuki metin (aydınlatma metni) bu sayfanın parçası değildir ve avukat onayı gerektirir (doğrulanmadı).

## Kısaca

- Agent sayar. AI aracının kendi log dosyalarını okur, bunları sayılara (token, adetler, bir konu etiketi) çevirir ve yalnızca bu sayıları gönderir.
- Prompt'larınız, AI'ın yanıtları, kaynak kod, dosya içerikleri, dosya adları, branch adları ve git kimlik bilgileri sunucuya gitmez, sunucuda saklanmaz.
- Agent proje dosyalarınızı açmaz. Proje klasöründe yalnızca git remote'unu okur ve onu `owner/repo` haline getirir.
- Bilgisayarınızdan bir şey çıkmadan önce tam olarak neyin gideceğini görebilirsiniz: `harnext-agent preview`.

## Agent bilgisayarınızda neyi okur

| Ne | Nerede | Neden |
|---|---|---|
| Claude Code oturum kayıtları (transcript) | `~/.claude/projects/**/*.jsonl` | Token sayıları, model, araç kullanım adetleri, friction sayaçları |
| VS Code Copilot sohbet oturumları | VS Code `workspaceStorage/<id>/chatSessions/*.jsonl` ve aynı çalışma alanının `workspace.json` dosyası | Friction sayaçları, konu, hangi proje klasörü |
| Copilot telemetri dosyası | Agent'ın kendi dosyası (varsayılan) veya sizin seçtiğiniz yol | Token, model, Copilot kredisi |
| Projenin git verisi | Çalışma klasörünün `.git`, `.git/commondir` ve `.git/config` dosyaları | Yalnızca remote adresi, `owner/repo` haline indirgenir. Kullanıcı adı, parola, token, branch ve sunucu adı atılır |
| İşaret dosyası adları (`package.json`, `go.mod` vb.) | Çalışma klasörü ve üst klasörleri | Yalnızca var olup olmadığına bakılır, dosya açılmaz. Git yoksa proje kökünü bulmak için kullanılır |
| Agent'ın kendi dosyaları | Agent klasörü (token, ayarlar, yerel veritabanı, durum) | Agent'ın çalışması |
| VS Code `settings.json` | Yalnızca `harnext-agent copilot --apply` komutunu çalıştırıp onay verdiğinizde | Copilot telemetrisini açar; dosya önce yedeklenir, `--revert` eski haline getirir |

Agent `.env`, `appsettings.json`, kaynak kod dosyalarını veya projelerinizdeki başka hiçbir dosyayı okumaz. Kendi değişkenleri (`HARNEXT_TOKEN`, `HARNEXT_HOME`, `HARNEXT_NO_AUTO_UPDATE`) ve ev ile yapılandırma klasörünüzü bulmaya yarayan standart değişkenler dışında ortam değişkenlerini okumaz. Başka programları yalnızca arka plan servisini kurmak veya durdurmak ve doğrulamış olduğu bir güncellemeyi başlatmak için çalıştırır.

Log dosyaları prompt'larınızı içerir, bu yüzden agent o satırları okumak zorundadır. Sayıları çıkarır, metni atar. Örneğin mesajınız bellekte kalıplarla karşılaştırılır (ör. "bu yanlış") ve bir pushback sayılır; mesajın kendisi tutulmaz. Bir düzenleme için dosya yolunun yalnızca proje içindeki klasör kısmı alınır, dosya adı asla.

Bu liste bir testle zorlanır: en sondaki "Nasıl doğrularsınız" bölümüne bakın.

## Agent neyi gönderir

Yalnızca kendi Harnext sunucunuza, HTTPS üzerinden ve agent token'ınızla:

| İstek | Ne taşır |
|---|---|
| `POST /api/v1/ingest` | Oturum başına: araç, proje kimliği, konu etiketi, sayaçlar, model ve gün başına token sayıları, zamanlar, oturumun aktif süresi (saniye, `active_seconds`) ve klasör kırılımı (aşağıya bakın) |
| `GET /api/v1/agent-config` | Token ve agent sürümü dışında sizinle ilgili bir şey yok. Sayaçları hesaplamak için kullanılan imzalı kuralları döner |
| `GET /api/v1/agent-release` ve sürüm paketi | Güncelleme kontrolü. Paketler imza ve sha256 ile doğrulanır |

Bir ingest isteği şu biçimdedir (kısaltılmış; tam şema [contracts/ingest.v1.schema.json](../contracts/ingest.v1.schema.json), eksiksiz örnek [contracts/testdata/ingest.v5.vector.json](../contracts/testdata/ingest.v5.vector.json)):

```json
{
  "payload_version": 5,
  "agent_version": "0.3.0",
  "config_version": 7,
  "sessions": [{
    "session_external_id": "claude-multi",
    "tool_name": "claude_code",
    "project": "acme/app",
    "project_path": "apps/web",
    "topic": "feature",
    "quality": { "exchanges": 10, "friction_exchanges": 1, "tool_calls": 10, "...": "yalnızca sayaçlar" },
    "turn_count": 10,
    "started_at": "2026-11-01T10:00:00Z",
    "ended_at": "2026-11-01T11:00:00Z",
    "active_seconds": 3000,
    "usage": [{ "model": "claude-sonnet-4", "day": "2026-11-01", "input_tokens": 2, "output_tokens": 1000, "...": "token sayıları" }],
    "folders": [{ "path": "apps/web", "turn_count": 6, "quality": { "...": "sayaçlar" }, "usage": [] }]
  }]
}
```

İçindeki her şey bir sayı, bir tarih, bir model adı, sabit bir etiket veya bir proje kimliğidir.

Proje kimliği hakkında: git deposunda `owner/repo` ve deponun içindeki klasördür (`apps/web`). Git dışında, ev klasörünüzün `~` ile değiştirildiği çalışma klasörüdür (ör. `~/work/client-app`). Ev klasörünüzün dışındaki bir klasör olduğu gibi gönderilir; bu yüzden böyle yollarda hassas isimlerden kaçının veya yöneticinizle nasıl ele alınacağını konuşun.

## Tek seferlik tarama ve paylaşılabilir kart

`harnext-agent scan` (veya `curl -fsSL app.harnext.ai/scan.sh | sh`) aynı yerel dosyaların son 30 gününü okur, hiçbir şey kurmaz, diske hiçbir şey yazmaz ve kendiliğinden hiçbir şey göndermez: ekrana bir özet yazar. Yalnızca evet derseniz göndereceği JSON'u (model başına token toplamları, oturum ve aktif gün sayıları; proje adları yalnızca ayrıca evet derseniz; şema: [contracts/card.v1.schema.json](../contracts/card.v1.schema.json)) birebir yazar ve bir kez daha sorar. Sunucu bunu 24 saat geçerli, size özel bir bağlantının arkasında taslak olarak tutar. Siz yayınlamadan hiçbir şey herkese açık olmaz: koltuk fiyatınızı girer, proje adlarının gösterilip gösterilmeyeceğini seçer (gösterilmezse kartlardan silinir) ve onaylarsınız. Yayınlanan kart kendi bağlantısında herkese açıktır ve yayınlarken bir kez gösterilen silme bağlantısıyla silene kadar kalır (hesap yoktur: o bağlantıyı saklayın). Bağlantılar rastgeledir ve yalnızca SHA-256 özetleri saklanır. Ağ adresleri oldukları gibi saklanmaz: sınırlama için anahtarlı bir özet tutulur ve iki saat sonra silinir. Kart token sayılarını ve güncel liste fiyatlarıyla bir tahmini gösterir; kullanıcı beyanı ve doğrulanmamış olarak işaretlidir, arama motorlarından dizinlememesi istenir.

## Asla gönderilmeyenler

Prompt'lar ve yanıtlar, AI'ın akıl yürütmesi, araç girdi ve çıktıları (komutlar, dosya içerikleri, hata metinleri), kaynak kod, dosya adları, ev klasörünüz içindeki tam yollar, kullanıcı adınız ve bilgisayar adınız, branch adları, git kimlik bilgileri, ortam değişkenleri, e-posta adresiniz (agent payload'unda e-posta alanı yoktur), ekran görüntüleri.

## Bilgisayarınızda kalanlar

Agent, sayaçları ve her oturumun tam çalışma klasörünü içeren yerel bir veritabanı tutar. `harnext-agent stats` bunu gösterir. Bilgisayarınızdan çıkmaz. Agent'ın durum dosyasında yalnızca sayılar ve zamanlar bulunur: yol yok, içerik yok.

## Sunucu bununla ne yapar

Sunucu yukarıdaki sayıları saklar, maliyeti ve skorları siz baktığınızda hesaplar, ham satırları organizasyonunuzun belirlediği saklama süresi boyunca (3 ile 36 ay, varsayılan 13) tutar, sonra siler. Yöneticiler kişileri değil organizasyon toplamlarını görür. Asgari grup eşiğinden (varsayılan 3) küçük bir grup gösterilmez. Kendi verinizi dışa aktarabilir ve üyeliğinizi silebilirsiniz.

## Nasıl doğrularsınız

1. **Göndermeden önce verinizi görün.** `harnext-agent preview`, en yeni oturumunuz için istek gövdesini JSON olarak yazar. `--sessions 5` daha fazlasını, `--all` hepsini gösterir. Komut yalnızca agent'ın kayıtlı durumunu okur, ağa hiçbir istek atmaz ve hiçbir şeyi değiştirmez. Çıktıda olmayan şey gönderilmez.
2. **Sahte sır testi.** Depo kökünde `go test ./internal/service -run TestNoFakeSecretLeavesTheMachine`: bir prompt'a, yanıta, düşünme bloğuna, shell komutuna, araç hatasına, `.env` ve `appsettings.json` dosyalarına, bir kaynak dosyaya, bir dosya adına, kimlik bilgili git remote'una, branch'e ve kullanıcı klasörü adına sahte sırlar yazar; hem Claude Code hem Copilot için. Agent'ı baştan sona çalıştırır ve bunlardan biri yüklemede, durum dosyasında veya logda görünürse başarısız olur.
3. **Dosya erişim testi.** `go test ./internal/fsaudit`: agent kodunun bir dosyayı okuduğu, klasör listelediği, ortam değişkeni okuduğu, program başlattığı veya ağ bağlantısı açtığı her yeri, nedeniyle birlikte listeler. Yeni biri bu test başarısız olmadan eklenemez; yani bilgisayarınıza daha fazla bakacak bir değişikliğin yazılı gerekçesi ve gözden geçirmesi olmak zorundadır.
4. **Sözleşme.** [contracts/ingest.v1.schema.json](../contracts/ingest.v1.schema.json) istek gövdesinin şemasıdır. Başka alana izin vermez.
5. **Agent'ın kendi görünümü.** `harnext-agent status`, konuştuğu sunucuyu ve en son ne yaptığını gösterir.

Bu kontrollerin sınırı: testler bu kodun davranışını kanıtlar, değiştirilmiş bir agent'ın değil. Testlerde yalnızca uydurma değerler kullanılır; gerçek prompt, anahtar veya müşteri verisi yoktur.
