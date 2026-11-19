package chat

import (
	"strings"
	"unicode"

	"github.com/cambrian-sh/core/domain"
)

// The language of a conversation, for the kernel's own fixed sentences.
//
// Owner decision (2026-11-07, T-20261107-chat-failure-texts-are-english-only-in-non):
// failure and fallback messages follow the conversation's language. They are NOT
// translated by a model: a failure is very often the moment the model is unavailable, so
// the sentence that reports it must not depend on one. Instead every fixed sentence is a
// message id with one entry per language in a catalogue, and the turn's language comes
// from a cheap deterministic detector over what the user wrote.
//
// Adding a language is data only: a languageProfile (so it can be detected) and a full
// catalogue entry (TestCatalogue_EveryDetectableLanguageIsComplete holds the two together).

// DefaultLanguage is the language used when nothing the user wrote identifies one and the
// operator configured none (execution.chat.chat_default_language).
const DefaultLanguage = "en"

// MessageID names one fixed, user-facing sentence the kernel says on a chat turn.
type MessageID string

const (
	MsgTurnStalled   MessageID = "turn_stalled"
	MsgEmptyReply    MessageID = "empty_reply"
	MsgTurnTimeout   MessageID = "turn_timeout"
	MsgTurnCancelled MessageID = "turn_cancelled"
	MsgAtCapacity    MessageID = "at_capacity"
	MsgSessionLost   MessageID = "session_lost"
	MsgGenericFail   MessageID = "generic_failure"
	// ENG-2124: the provider and restart classes of a failed or interrupted turn.
	MsgProviderAuth        MessageID = "provider_auth"
	MsgProviderBusy        MessageID = "provider_busy"
	MsgProviderUnavailable MessageID = "provider_unavailable"
	MsgNotConfigured       MessageID = "not_configured"
	MsgSpendLimit          MessageID = "spend_limit"
	MsgContextTooLarge     MessageID = "context_too_large"
	MsgAssistantRestarted  MessageID = "assistant_restarted"
	// The ADR-0145 D8 reply check's gap statements (enforce mode, after the one
	// retry): {values} is the uncited values as written, {items} the unanswered
	// checklist items in the person's own words.
	MsgReplyUnverified MessageID = "reply_unverified"
	MsgReplyUncovered  MessageID = "reply_uncovered"
	// MsgReplySource names, after the one retry, the organisation documents a value
	// stated from recalled memory comes from when the reply still does not
	// (T-20261109-chat-agent-answers-a-published-rule): {sources}.
	MsgReplySource MessageID = "reply_source"
)

// allMessageIDs is the closed set every catalogue language must cover.
var allMessageIDs = []MessageID{
	MsgTurnStalled, MsgEmptyReply, MsgTurnTimeout, MsgTurnCancelled,
	MsgAtCapacity, MsgSessionLost, MsgGenericFail,
	MsgProviderAuth, MsgProviderBusy, MsgProviderUnavailable, MsgNotConfigured, MsgSpendLimit,
	MsgContextTooLarge, MsgAssistantRestarted,
	MsgReplyUnverified, MsgReplyUncovered, MsgReplySource,
}

// replyCheckMessageIDs are the catalogue's non-failure sentences (the reply
// check's gap statements); every other id is a turn failure humanFailure says.
var replyCheckMessageIDs = []MessageID{MsgReplyUnverified, MsgReplyUncovered, MsgReplySource}

// messageCatalogue holds every fixed failure sentence, keyed by language then message id.
// The English entries are the strings humanFailure returned before the catalogue existed,
// byte for byte, so an English conversation sees no change.
var messageCatalogue = map[string]map[MessageID]string{
	"en": {
		MsgTurnStalled:         "That request stopped making progress, so I stopped waiting on it. Worth trying again.",
		MsgEmptyReply:          "I wasn't able to produce an answer for that one. Try rephrasing it?",
		MsgTurnTimeout:         "That took longer than I'm allowed to spend on one message, so I stopped. A narrower request usually gets through.",
		MsgTurnCancelled:       "That was cancelled before I finished.",
		MsgAtCapacity:          "I'm at capacity right now. Give me a moment and try again.",
		MsgSessionLost:         "I lost my working session part-way through that one. Trying again usually works.",
		MsgGenericFail:         "Something went wrong on my side and I couldn't finish that one.",
		MsgProviderAuth:        "The AI service rejected our credentials. An operator needs to check the model settings.",
		MsgProviderBusy:        "The AI service is busy right now. Try again in a minute.",
		MsgProviderUnavailable: "The AI service can't be reached right now. Try again in a few minutes.",
		MsgNotConfigured:       "The AI service isn't set up for this assistant yet, so it can't answer. An operator needs to check the model settings.",
		MsgSpendLimit:          "This conversation has reached its AI usage limit. An operator can raise it.",
		MsgContextTooLarge:     "This conversation has grown too long for the AI model to read at once. Starting a new conversation will help.",
		MsgAssistantRestarted:  "This turn was interrupted because the assistant restarted. Please send your message again.",
		MsgReplyUnverified:     "Note: I could not confirm these against the records read for this request: {values}.",
		MsgReplyUncovered:      "Not covered in this answer: {items}.",
		MsgReplySource:         "Source in the organisation's documents: {sources}.",
	},
	"tr": {
		MsgTurnStalled:         "Bu istek ilerlemeyi bıraktı, bu yüzden beklemeyi bıraktım. Yeniden denemeye değer.",
		MsgEmptyReply:          "Bu isteğe bir yanıt üretemedim. Farklı bir şekilde sormayı dener misiniz?",
		MsgTurnTimeout:         "Bu, tek bir mesaja ayırabileceğim süreden uzun sürdü, bu yüzden durdum. Daha dar kapsamlı bir istek genellikle sonuçlanır.",
		MsgTurnCancelled:       "Bu istek ben bitirmeden iptal edildi.",
		MsgAtCapacity:          "Şu anda kapasitem dolu. Biraz bekleyip yeniden deneyin.",
		MsgSessionLost:         "Bu isteğin ortasında çalışma oturumumu kaybettim. Yeniden denemek genellikle işe yarar.",
		MsgGenericFail:         "Benim tarafımda bir sorun oluştu ve bu isteği tamamlayamadım.",
		MsgProviderAuth:        "Yapay zekâ hizmeti kimlik bilgilerimizi reddetti. Bir yöneticinin model ayarlarını kontrol etmesi gerekiyor.",
		MsgProviderBusy:        "Yapay zekâ hizmeti şu anda yoğun. Bir dakika sonra yeniden deneyin.",
		MsgProviderUnavailable: "Yapay zekâ hizmetine şu anda ulaşılamıyor. Birkaç dakika sonra yeniden deneyin.",
		MsgNotConfigured:       "Yapay zekâ hizmeti bu asistan için henüz yapılandırılmadı, bu yüzden yanıt veremiyor. Bir yöneticinin model ayarlarını kontrol etmesi gerekiyor.",
		MsgSpendLimit:          "Bu konuşma yapay zekâ kullanım sınırına ulaştı. Bir yönetici sınırı yükseltebilir.",
		MsgContextTooLarge:     "Bu konuşma, yapay zekâ modelinin tek seferde okuyabileceğinden uzun hale geldi. Yeni bir konuşma başlatmak işe yarar.",
		MsgAssistantRestarted:  "Asistan yeniden başlatıldığı için bu istek yarıda kaldı. Lütfen mesajınızı yeniden gönderin.",
		MsgReplyUnverified:     "Not: Şu değerleri bu istek için okunan kayıtlarla doğrulayamadım: {values}.",
		MsgReplyUncovered:      "Bu yanıtta karşılanmayan kısımlar: {items}.",
		MsgReplySource:         "Kurum belgelerindeki kaynak: {sources}.",
	},
}

// SupportedLanguage reports whether the catalogue has entries for lang.
func SupportedLanguage(lang string) bool {
	_, ok := messageCatalogue[normaliseLanguage(lang)]
	return ok
}

// Message returns the fixed sentence id in lang, falling back to English (and, should even
// that be missing, to the empty string — the caller then shows nothing rather than an id).
func Message(id MessageID, lang string) string {
	if m, ok := messageCatalogue[normaliseLanguage(lang)]; ok {
		if s, ok := m[id]; ok {
			return s
		}
	}
	return messageCatalogue[DefaultLanguage][id]
}

func normaliseLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	// "tr-TR", "en_GB" -> the primary subtag; the catalogue is keyed by it.
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	return lang
}

// languageProfile is what makes a language detectable: letters that (among the catalogue's
// languages) only it uses, and short function words that are frequent in it and rare in
// the others. Both are deliberately small; the detector only has to pick among the
// catalogue's languages, not identify any language in the world.
type languageProfile struct {
	code string
	// letters are distinctive to this language among the catalogue's (2 points each);
	// weakLetters are shared with languages the catalogue may later add (1 point each).
	letters     string
	weakLetters string
	words       []string
}

var languageProfiles = []languageProfile{
	{
		code: "en",
		words: []string{"the", "and", "is", "are", "you", "your", "what", "how", "can", "could",
			"please", "my", "i", "it", "to", "of", "for", "with", "this", "that", "do", "does",
			"hello", "hi", "thanks", "thank", "yes", "no", "about", "me", "have", "where", "when", "why"},
	},
	{
		code: "tr",
		// ğ ş ı İ are Turkish-only among the catalogue's languages; ç ö ü are not (French,
		// German), so they weigh less.
		letters:     "ğĞşŞıİ",
		weakLetters: "çÇöÖüÜ",
		words: []string{"ve", "bir", "bu", "şu", "için", "icin", "mi", "mı", "mu", "mü", "ne",
			"hangi", "hangisi", "nedir", "nerede", "neler", "kim", "nasıl", "nasil",
			"lütfen", "lutfen", "merhaba", "teşekkürler", "tesekkurler",
			"teşekkür", "tesekkur", "evet", "hayır", "hayir", "var", "yok", "değil", "degil",
			"ile", "ama", "çok", "cok", "daha", "benim", "bana", "beni", "neden",
			"selam", "olarak", "gibi", "kadar", "misiniz", "musunuz", "miyim", "istiyorum"},
	},
}

// DetectLanguage returns the catalogue language text is most likely written in, or "" when
// nothing in it is distinctive enough to say (a number, "ok", an emoji, a tie).
//
// Deterministic and cheap: distinctive letters count double, shared letters and function
// words once. It is a
// choice among the catalogue's languages, nothing more.
func DetectLanguage(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	scores := make(map[string]int, len(languageProfiles))
	for _, p := range languageProfiles {
		for _, r := range text {
			switch {
			case strings.ContainsRune(p.letters, r):
				scores[p.code] += 2
			case strings.ContainsRune(p.weakLetters, r):
				scores[p.code]++
			}
		}
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	})
	for _, w := range words {
		w = strings.Trim(w, "'")
		for _, p := range languageProfiles {
			for _, pw := range p.words {
				if w == pw {
					scores[p.code]++
					break
				}
			}
		}
	}
	best, bestScore, tie := "", 0, false
	for _, p := range languageProfiles {
		switch s := scores[p.code]; {
		case s > bestScore:
			best, bestScore, tie = p.code, s, false
		case s == bestScore && s > 0:
			tie = true
		}
	}
	if bestScore == 0 || tie {
		return ""
	}
	return best
}

// turnLanguage is the language of the turn being answered: the latest user message's, else
// the most recent earlier user message's that identifies one (a Turkish conversation that
// says "ok" is still Turkish), else the configured default, else English.
func (s *TurnService) turnLanguage(latest string, history []domain.Message) string {
	if lang := DetectLanguage(latest); lang != "" {
		return lang
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != domain.MessageRoleUser {
			continue
		}
		if lang := DetectLanguage(history[i].Content); lang != "" {
			return lang
		}
	}
	if SupportedLanguage(s.DefaultLanguage) {
		return normaliseLanguage(s.DefaultLanguage)
	}
	return DefaultLanguage
}
