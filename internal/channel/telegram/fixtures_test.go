package telegram

// Checked-in Bot API update fixtures (#251). No real Telegram calls: these
// payloads stand in for the wire format of media messages, captions, edits,
// and the unhandled kinds that must never vanish silently.

// photoUpdate is a private-chat photo with a caption.
const photoUpdate = `{"ok":true,"result":[
	{"update_id":101,"message":{
		"message_id":1001,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"photo":[
			{"file_id":"small_id","file_unique_id":"sm1","width":90,"height":90,"file_size":1200},
			{"file_id":"photo_id","file_unique_id":"ph1","width":640,"height":480,"file_size":81920}
		],
		"caption":"the washing machine display"
	}}
]}`

// documentUpdate is a private-chat document with a caption.
const documentUpdate = `{"ok":true,"result":[
	{"update_id":102,"message":{
		"message_id":1002,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"document":{
			"file_id":"doc_id","file_unique_id":"dc1",
			"file_name":"washing-machine-manual.pdf",
			"mime_type":"application/pdf","file_size":2048
		},
		"caption":"the manual"
	}}
]}`

// voiceUpdate is a private-chat voice note without a caption.
const voiceUpdate = `{"ok":true,"result":[
	{"update_id":103,"message":{
		"message_id":1003,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"voice":{"file_id":"voice_id","file_unique_id":"vc1","duration":3,"mime_type":"audio/ogg","file_size":4096}
	}}
]}`

// videoUpdate is a private-chat video without a caption.
const videoUpdate = `{"ok":true,"result":[
	{"update_id":104,"message":{
		"message_id":1004,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"video":{
			"file_id":"video_id","file_unique_id":"vd1","width":640,"height":360,
			"duration":5,"mime_type":"video/mp4","file_size":16384
		}
	}}
]}`

// audioUpdate is a private-chat audio file with a filename.
const audioUpdate = `{"ok":true,"result":[
	{"update_id":105,"message":{
		"message_id":1005,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"audio":{
			"file_id":"audio_id","file_unique_id":"au1",
			"file_name":"beep.mp3","mime_type":"audio/mpeg","file_size":32768
		}
	}}
]}`

// videoNoteUpdate is a private-chat round video note.
const videoNoteUpdate = `{"ok":true,"result":[
	{"update_id":106,"message":{
		"message_id":1006,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"video_note":{"file_id":"vn_id","file_unique_id":"vn1","length":240,"duration":4,"file_size":65536}
	}}
]}`

// oversizedPhotoUpdate carries a file_size over any conservative cap; the
// adapter must refuse it before any fetch, not after.
const oversizedPhotoUpdate = `{"ok":true,"result":[
	{"update_id":107,"message":{
		"message_id":1007,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"photo":[
			{"file_id":"huge_id","file_unique_id":"hg1","width":1280,"height":960,"file_size":500000000}
		]
	}}
]}`

// editedMessageUpdate is a message edit. The original was already handled,
// so the edit is deliberately ignored — but logged, never silent.
const editedMessageUpdate = `{"ok":true,"result":[
	{"update_id":109,"edited_message":{
		"message_id":1001,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":100,"type":"private"},
		"date":1700000000,
		"edit_date":1700000001,
		"text":"the washing machine display (fixed)"
	}}
]}`

// groupPhotoMentionUpdate is a supergroup photo whose caption mentions the
// bot via caption_entities — the mention gate must apply to captions too.
const groupPhotoMentionUpdate = `{"ok":true,"result":[
	{"update_id":110,"message":{
		"message_id":1010,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":-100,"type":"supergroup"},
		"date":1700000000,
		"photo":[
			{"file_id":"gphoto_id","file_unique_id":"gp1","width":640,"height":480,"file_size":8192}
		],
		"caption":"@waffle_bot look at this",
		"caption_entities":[{"type":"mention","offset":0,"length":11}]
	}}
]}`

// groupPhotoNoMentionUpdate is a supergroup photo whose caption does not
// mention the bot: the attachment must not bypass the mention gate.
const groupPhotoNoMentionUpdate = `{"ok":true,"result":[
	{"update_id":111,"message":{
		"message_id":1011,
		"from":{"id":42,"first_name":"Matt"},
		"chat":{"id":-100,"type":"supergroup"},
		"date":1700000000,
		"photo":[
			{"file_id":"gphoto2_id","file_unique_id":"gp2","width":640,"height":480,"file_size":8192}
		],
		"caption":"just sharing"
	}}
]}`
