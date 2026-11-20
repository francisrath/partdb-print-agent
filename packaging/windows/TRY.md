# Testing a label printer with pdb-print

Thanks for helping! This takes about 10 minutes and uses about 5–6 labels.

1. Unzip this folder anywhere (for example on the desktop).
2. Make sure the printer is switched on, connected to the same network as this PC (network
   cable or Wi-Fi; USB isn't supported on Windows yet), and has labels loaded. You don't need to stand next to it: all test labels print in one go, then
   you answer questions about them. Only a fix (a reprint) or the last step (opening the lid) needs
   another trip to the printer.
3. Double-click **try-printer.cmd**. Windows may say it protected your PC: click
   **More info**, then **Run anyway** (the program isn't signed yet).
4. Answer each question with **y** (yes) or **n** (no) and Enter. Enter alone takes the answer
   shown in capitals. Every label has a number on it (1/5, 2/5 …) so you can tell them apart.
5. At the end it writes a file named `pdb-print-try-….txt` in this folder. Send that file and a
   photo of all the printed labels to Francis.

To stop early, press Ctrl+C. If Windows then asks "Terminate batch job (Y/N)?", answer **N** so the
window stays open and you can see where the file was saved.

The program never saves anything on the printer. While it runs it changes a few page settings
(print width, label length, position, mirror, upside-down) that only last until the printer is
switched off; at the end it puts back the ones the printer reported and tells you which. Switching
the printer off and on loads the settings saved on it, so if you changed something on the printer
without saving it, note it down before the test. It doesn't install anything.
