package protocol

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// frozenVectors 是 protocol-vectors.json 的逐字副本（sha256
// 0b4957b5d66cb969b0398d940913482b3eec048e0737c8c59a6b86948bc1189f）。
// 独立自包含，避免测试依赖工作区外路径。
const frozenVectors = `{
  "description": "PUBLIC TEST VECTOR ONLY: deterministic seed bytes 0..31, never use for real identities",
  "publicKey": "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=",
  "testSeedBytes": [0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31],
  "vectors": [
    {
      "canonicalHex": "00000010484e49454f4a2d454e524f4c4c2d5631000000126a756467652e6578616d706c652e746573740000000b6368616c6c656e67652d3100000008626d39755932553d00000008656e726f6c6c2d3100000009e88a82e782b9e4b8800000002c41364548762f504f454c3464634e3059353076416d57666b316a436270513166486479475a424a564d62673d0000004064336138633638366238306663303138393135396437666462623930353162663633383666356633376531636632323734316430323261653733666330613733",
      "fields": ["HNIEOJ-ENROLL-V1","judge.example.test","challenge-1","bm9uY2U=","enroll-1","节点一","A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=","d3a8c686b80fc0189159d7fdbb9051bf6386f5f37e1cf22741d022ae73fc0a73"],
      "signature": "+BneZttSelKbxoMZqrjPf3vnLJYXeG21yGvfZ3LvYl4sQSSG7/lELHlcRJ5n8PhIhhfuCYkyfAgWnm8KtjsGCQ=="
    },
    {
      "canonicalHex": "0000000e484e49454f4a2d415554482d5631000000126a756467652e6578616d706c652e746573740000000b6368616c6c656e67652d3200000008626d39755932553d000000066e6f64652d31000000056b65792d31",
      "fields": ["HNIEOJ-AUTH-V1","judge.example.test","challenge-2","bm9uY2U=","node-1","key-1"],
      "signature": "enFSUNlv7ECjYuQynGdvALz9C/ek2gca7Ti2VilpuWZOUfVrkHmvNhtGup6pH0loWykhUQkt95kv7elqDd+FDA=="
    },
    {
      "canonicalHex": "0000000e484e49454f4a2d485454502d5631000000126a756467652e6578616d706c652e74657374000000066e6f64652d31000000056b65792d3100000003474554000000462f6a756467652f70726f626c656d732f312f74657374646174613f7375626d697373696f6e49643d7331266a756467655461736b49643d6a3126617474656d707449643d613100000040653362306334343239386663316331343961666266346338393936666239323432376165343165343634396239333463613439353939316237383532623835350000000d313738393738363030303030300000000c6e6f6e63652d687474702d31",
      "fields": ["HNIEOJ-HTTP-V1","judge.example.test","node-1","key-1","GET","/judge/problems/1/testdata?submissionId=s1&judgeTaskId=j1&attemptId=a1","e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","1789786000000","nonce-http-1"],
      "signature": "sUwZnWuq44abymSnFoO0dmFCxG8jSZYzEGZNBFCIMhW/L592WWKJ+x194k5QiGne0wVTz3+nftftU2feaJE1Bw=="
    },
    {
      "canonicalHex": "00000018484e49454f4a2d524f544154452d505245504152452d5631000000126a756467652e6578616d706c652e74657374000000066e6f64652d310000000a726f746174696f6e2d310000002c41364548762f504f454c3464634e3059353076416d57666b316a436270513166486479475a424a564d62673d",
      "fields": ["HNIEOJ-ROTATE-PREPARE-V1","judge.example.test","node-1","rotation-1","A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="],
      "signature": "YlXFrWGwznk4epyo0+51QgcwsVUGkmXAYVEn1PWmIHw+wkAwFFUA20PTafpoAqChJqLW2zcKsDbhlepr841bDg=="
    },
    {
      "canonicalHex": "00000018484e49454f4a2d524f544154452d434f4e4649524d2d5631000000126a756467652e6578616d706c652e74657374000000066e6f64652d310000000a726f746174696f6e2d31000000056b65792d320000000d6e6f6e63652d636f6e6669726d",
      "fields": ["HNIEOJ-ROTATE-CONFIRM-V1","judge.example.test","node-1","rotation-1","key-2","nonce-confirm"],
      "signature": "s6vB7suSpmVIhBMUlX81dVFyi8DgjkDQP0Fmu+igJy+dmQ9FDHd6vVcBX6vsrISRBK8yVI9nj/pCTbTOT7xCAw=="
    }
  ]
}`

type vectorFile struct {
	PublicKey     string `json:"publicKey"`
	TestSeedBytes []byte `json:"testSeedBytes"`
	Vectors       []struct {
		CanonicalHex string   `json:"canonicalHex"`
		Fields       []string `json:"fields"`
		Signature    string   `json:"signature"`
	} `json:"vectors"`
}

func TestFrozenVectors(t *testing.T) {
	var file vectorFile
	if err := json.Unmarshal([]byte(frozenVectors), &file); err != nil {
		t.Fatalf("parse frozen vectors: %v", err)
	}
	if len(file.TestSeedBytes) != ed25519.SeedSize {
		t.Fatalf("seed must be %d bytes, got %d", ed25519.SeedSize, len(file.TestSeedBytes))
	}
	privateKey := ed25519.NewKeyFromSeed(file.TestSeedBytes)
	if got := EncodePublicKey(privateKey.Public().(ed25519.PublicKey)); got != file.PublicKey {
		t.Fatalf("derived public key %q != vector %q", got, file.PublicKey)
	}
	if len(file.Vectors) != 5 {
		t.Fatalf("expected 5 vectors, got %d", len(file.Vectors))
	}
	for i, vector := range file.Vectors {
		canonical := Canonical(vector.Fields...)
		if got := hex.EncodeToString(canonical); got != vector.CanonicalHex {
			t.Fatalf("vector %d canonical mismatch:\n got %s\nwant %s", i, got, vector.CanonicalHex)
		}
		if err := Verify(file.PublicKey, vector.Signature, vector.Fields...); err != nil {
			t.Fatalf("vector %d verify: %v", i, err)
		}
		// 篡改任意一个字段必须失败。
		tampered := append([]string(nil), vector.Fields...)
		tampered[len(tampered)-1] += "x"
		if err := Verify(file.PublicKey, vector.Signature, tampered...); err == nil {
			t.Fatalf("vector %d tampered field verified unexpectedly", i)
		}
	}
}

func TestCanonicalEncodesLengthPrefixes(t *testing.T) {
	got := hex.EncodeToString(Canonical("", "AB"))
	want := "00000000" + "00000002" + "4142"
	if got != want {
		t.Fatalf("canonical got %s want %s", got, want)
	}
}

func TestBootstrapDigestUsesLowercaseHex(t *testing.T) {
	if got := BootstrapDigest("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("bootstrap digest mismatch: %s", got)
	}
}
