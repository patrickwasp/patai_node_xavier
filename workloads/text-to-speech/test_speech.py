"""The speech service's tests, in its image, with the voices in /models: long answers in pieces on each engine, and
the language each is read in (workloads/text-to-speech/test runs them).

  python3 -I -B test_speech.py      next to patai-speech, with its manifest at /etc/patai-speech/manifest.json
"""
import http.server
import importlib.machinery
import importlib.util
import io
import json
import os
import re
import threading
import unittest
import urllib.error
import urllib.request
import wave

HERE = os.path.dirname(os.path.abspath(__file__))
loader = importlib.machinery.SourceFileLoader("speech", os.path.join(HERE, "patai-speech"))
speech = importlib.util.module_from_spec(importlib.util.spec_from_loader("speech", loader))
loader.exec_module(speech)

# An answer of three paragraphs, as Chat reads it.
PARAGRAPHS = (
    "The backup finished at 3:40 this morning and took 12 minutes, which is about what it took last week. "
    "Everything in Chat was in it: every person's chats, their files, their memories and the settings they chose, "
    "along with the models' list and the unit's own configuration, so a restore would bring all of it back. "
    "Nothing was sent anywhere else.\n\n"
    "If you want it to run at a different time, open Settings, then Backups, and choose another hour; the next one "
    "runs then, and the one after that a day later, unless the unit is off, in which case it runs as soon as it's back."
    "\n\nOne thing to know: the disk you back up to is 82% full, and at this rate it'll be full in about three weeks, "
    "so it may be worth removing older backups or using a bigger disk before then."
)
# One sentence of 1,300 characters without a mark in it.
RUN_ON = ("the unit keeps every chat on its own disk and reads each answer aloud in the voice for its language "
          "and never sends any of it to another computer unless you ask it to and even then it says where it went "
          "and when and what was in it ") * 6
# Markdown as a client of the API may send it, a list and a code block in it.
LIST = (
    "Here's what to pack:\n\n- A warm jacket\n- Two pairs of boots\n- Sunscreen, even in winter\n- The map\n\n"
    "And to check the forecast:\n\n```sh\ncurl https://wttr.in/montreal?format=3\n```\n\nThat's all."
)
SPANISH = ("La copia de seguridad terminó esta mañana a las tres y cuarenta y tardó doce minutos sin que nada saliera "
           "de la unidad y la próxima empieza mañana a la misma hora salvo que la unidad esté apagada ") * 4


class Pieces(unittest.TestCase):
    def check(self, text, budget):
        found = speech.pieces(text, budget)
        self.assertTrue(all(0 < len(piece) <= budget for piece, _ in found), found)
        self.assertEqual("".join(piece for piece, _ in found).replace(" ", ""), re.sub(r"\s+", "", text))
        self.assertIsNone(found[-1][1])
        return found

    def test_short_text_is_one_piece(self):
        self.assertEqual(speech.pieces("  Yes, it's done.  ", 150), [["Yes, it's done.", None]])

    def test_paragraphs_cut_at_sentences(self):
        found = self.check(PARAGRAPHS, 150)
        self.assertIn("sentence", [cut for _, cut in found])
        # Sentences that fit together are read together.
        self.assertTrue(any(re.search(r"[.!?] \S", piece) for piece, _ in self.check(PARAGRAPHS, 400)))

    def test_long_sentence_cut_at_clauses_then_words(self):
        found = self.check(PARAGRAPHS.split(". ")[1], 60)
        self.assertEqual(found[0][1], "clause")
        self.assertEqual({cut for _, cut in self.check(RUN_ON, 150)}, {"word", None})

    def test_word_longer_than_the_budget(self):
        found = self.check("a" * 25 + " b", 10)
        self.assertEqual([piece for piece, _ in found], ["a" * 10, "a" * 10, "a" * 5 + " b"])

    def test_list_cut_at_its_lines(self):
        found = self.check(LIST, 40)
        self.assertTrue(any("Two pairs of boots" in piece for piece, _ in found), found)


class Engines(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        speech.load("/etc/patai-speech/manifest.json")
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), speech.Handler)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def say(self, text, engine, **more):
        body = json.dumps({"input": text, "voice": "auto", "engine": engine, **more}).encode()
        asked = urllib.request.Request(f"{self.base}/v1/audio/speech", body, {"Content-Type": "application/json"})
        with urllib.request.urlopen(asked, timeout=600) as answer:
            sound = wave.open(io.BytesIO(answer.read()))
            rate = sound.getframerate()
            samples = speech.np.frombuffer(sound.readframes(sound.getnframes()), dtype="<i2") / 32767
        loud = speech.np.flatnonzero(speech.np.abs(samples) > speech.QUIET)
        # The longest silence between the first sound and the last, in seconds.
        self.gap = speech.np.diff(loud).max(initial=0) / rate
        return answer.headers["X-Patai-Voice"], len(samples) / rate

    def read(self, text, engine, language="en", **more):
        voice, seconds = self.say(text, engine, **more)
        self.assertEqual(voice, f"{engine}-{language}")
        # All of it read: a voice says 11 to 20 characters a second, with pauses.
        self.assertGreater(seconds, len(re.sub(r"\W+", " ", text)) / 25, (engine, text[:40]))
        self.assertLess(seconds, len(text) / 8, (engine, text[:40]))
        # Pieces join with a pause like their cut's, not each one's own silence on both sides.
        self.assertLess(self.gap, 0.6, (engine, text[:40]))
        return seconds

    def test_long_answers_on_each_engine(self):
        for engine in speech.ENGINES:
            for text in (PARAGRAPHS, RUN_ON, LIST):
                with self.subTest(engine=engine, text=text[:30]):
                    self.read(text, engine)

    def test_longest_input_on_each_engine(self):
        longest = (RUN_ON * 4)[:speech.LONGEST].rsplit(" ", 1)[0]
        for engine in speech.ENGINES:
            with self.subTest(engine=engine):
                self.read(longest, engine)

    def test_language_of_a_long_answer(self):
        for engine in speech.ENGINES:
            with self.subTest(engine=engine):
                self.read(SPANISH, engine, "es")
                # A short one by the text around it, and nothing to tell by in the voice's own.
                self.assertEqual(self.say("Sí.", engine, context=SPANISH[:300])[0], f"{engine}-es")
                self.assertEqual(self.say("12, 40, 2026.", engine)[0], f"{engine}-en")

    def test_numbers_beyond_a_voice_s_limit(self):
        # 144 characters, within sano's English budget, but 18 numbers of seven digits are more phonemes than it takes.
        numbers = " ".join(str(1000003 + 7919 * n) for n in range(18))
        for engine in speech.ENGINES:
            with self.subTest(engine=engine):
                voice, seconds = self.say(numbers, engine)
                self.assertEqual(voice, f"{engine}-en")
                self.assertGreater(seconds, 30)

    def test_nothing_to_say(self):
        for engine in speech.ENGINES:
            with self.subTest(engine=engine):
                self.assertLess(self.say("``` - ```", engine)[1], 2)

    def test_piper_voices_let_go_free_their_memory(self):
        def anon():
            with open("/sys/fs/cgroup/memory.stat") as f:
                return next(int(line.split()[1]) for line in f if line.startswith("anon ")) >> 20
        if not os.path.exists("/sys/fs/cgroup/memory.stat"):
            self.skipTest("no cgroup v2 here")
        used = []
        for language in ("en", "es", "de", "fr", "it"):
            self.say(RUN_ON, "piper", language=language)
            used.append(anon())
        # Two voices are kept; one that's let go takes its memory with it. Without, each kept 300 MiB more.
        self.assertLess(used[-1] - used[1], 250, used)

    def test_slower_is_longer(self):
        normal = self.say(PARAGRAPHS, "sano")[1]
        self.assertGreater(self.say(PARAGRAPHS, "sano", speed=0.5)[1], normal * 1.6)


if __name__ == "__main__":
    unittest.main(verbosity=2)
