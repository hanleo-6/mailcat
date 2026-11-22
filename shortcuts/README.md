# mailcat iOS Shortcuts

Setup notes for the two iOS Shortcuts described in [SPEC.md](../SPEC.md#9-ios-shortcuts).

## Prerequisites (LAN / Hotspot mode)

1. Make sure `mailcat` is listening on your laptop's LAN (or hotspot) IP in `~/.config/mailcat/config.json`, not just `127.0.0.1:8765`.
   Find your IP with `ip -4 -br addr`, then update `listen`, e.g.:
   ```json
   "listen": ["127.0.0.1:8765", "192.168.1.20:8765"]
   ```
   (Later in M10 with Tailscale, you can switch `listen` back to `["127.0.0.1:8765"]`.)
2. Start the server on your laptop:
   ```fish
   go run ./cmd/mailcat serve
   ```
3. Grab your bearer token:
   ```fish
   go run ./cmd/mailcat token
   ```

---

## Send to mailcat

### 1. Create the Shortcut
Open the **Shortcuts** app on your iPhone, tap **+**, and name it **Send to mailcat**.

### 2. Enable Share Sheet
1. Tap the **ⓘ** (Details) icon at the bottom.
2. Enable **Show in Share Sheet**.
3. Tap the **Receive ... from Share Sheet** header at the top of the editor and select:
   - **Images**
   - **Media**
   - **Files**
   - **PDFs**
   (Tap **Done**.)

### 3. Add Actions (in order)

1. **Text**
   - Value: `http://<hostname>.local:8765` (no trailing slash; use `https://<host>.<tailnet>.ts.net` once on Tailscale)
2. **Set Variable**
   - Variable Name: `Host`
   - Input: `Text` (from step 1)
3. **Text**
   - Value: `<your mailcat token>`
4. **Set Variable**
   - Variable Name: `Token`
   - Input: `Text` (from step 3)
5. **Count**
   - Count `Items` in `Shortcut Input`
6. **Repeat with Each** item in `Shortcut Input`
   - Inside the loop:
     1. **Get Contents of URL**
        - URL: `[Host]/upload` (tap the URL field, insert the `Host` variable, then type `/upload`)
        - Expand the arrow (**>**):
          - **Method**: `POST`
          - **Headers**: Add key `Authorization`, value `Bearer [Token]` (note the space after `Bearer`)
          - **Request Body**: `Form` (tap `JSON` to switch it to `Form`)
          - **Add new field** → choose **File**:
            - Key: `file`
            - Value: `Repeat Item`
     2. **If** `Contents of URL` (tap the variable pill and set **Type** to **Text**) **does not contain** `saved`
        - **Show Alert**
          - Title: `mailcat upload failed`
          - Message: `Contents of URL`
          - (Optional: uncheck *Show Cancel Button*)
        - **Stop This Shortcut**
     3. **End If**
7. **End Repeat**
8. **Show Notification**
   - Body: `Sent [Count] to mailcat`

### 4. Exporting (without token)
Once tested, duplicate the Shortcut, clear the `Token` Text action, and export it via **Share → Export File** into `shortcuts/Send to mailcat.shortcut` (which you can send to your laptop using mailcat itself!).

### 5. Test checklist (M4)
- [ ] Share a photo from Photos → arrives in `inbox_dir`, desktop notification fires.
- [ ] Share a PDF from Files → arrives intact.
- [ ] Share a ~1-minute video → arrives intact.
- [ ] Temporarily change `Token` to a wrong value → phone shows the failure alert with `{"error":"unauthorized"}`.
- [ ] Record observations below:
  - **Photo format received:** _TBD (HEIC or JPEG?)_
  - **Filenames preserved:** _TBD (original names or generic?)_
