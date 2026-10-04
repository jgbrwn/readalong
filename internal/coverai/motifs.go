package coverai

import "fmt"

var allowedCoverMotifs = map[string]bool{
	"four_sisters": true, "family": true, "portrait": true, "open_book": true,
	"house": true, "city": true, "forest": true, "mountain": true, "ship": true,
	"flower": true, "bird": true, "horse": true, "tree": true, "lantern": true,
	"moon": true, "key": true, "crown": true, "sword": true, "mask": true,
	"abstract": true,
}

func motifLabel(motif string) string {
	labels := map[string]string{
		"four_sisters": "four sisters", "family": "a family", "portrait": "a portrait",
		"open_book": "an open book", "house": "a house", "city": "a city",
		"forest": "a forest", "mountain": "a mountain landscape", "ship": "a ship",
		"flower": "a flower", "bird": "a bird", "horse": "a horse", "tree": "a tree",
		"lantern": "a lantern", "moon": "the moon", "key": "a key", "crown": "a crown",
		"sword": "a sword", "mask": "theater masks", "abstract": "abstract forms",
	}
	if label := labels[motif]; label != "" {
		return label
	}
	return "a literary illustration"
}

func motifDescription(motif string) string {
	if motif == "four_sisters" {
		return "four distinct young women standing together, with varied hair and dress silhouettes"
	}
	return motifLabel(motif)
}

func motifArt(motif, accent, secondary string) string {
	switch motif {
	case "four_sisters":
		return peopleArt(accent, secondary, []personPosition{
			{x: 125, y: 275, scale: .84, hair: 0, class: "sister"},
			{x: 275, y: 255, scale: .88, hair: 1, class: "sister"},
			{x: 425, y: 275, scale: .84, hair: 2, class: "sister"},
			{x: 575, y: 292, scale: .80, hair: 3, class: "sister"},
		})
	case "family":
		return peopleArt(accent, secondary, []personPosition{
			{x: 245, y: 278, scale: .94, hair: 0, class: "family-member"},
			{x: 365, y: 255, scale: 1.08, hair: 1, class: "family-member"},
			{x: 485, y: 278, scale: .94, hair: 2, class: "family-member"},
		})
	case "portrait":
		return peopleArt(accent, secondary, []personPosition{
			{x: 350, y: 275, scale: 1.28, hair: 1, class: "portrait"},
		})
	case "open_book":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="4" stroke-linejoin="round" opacity=".84">
<path d="M350 487c-57-45-132-58-218-38V210c78-18 155-3 218 43 63-46 140-61 218-43v239c-86-20-161-7-218 38Z" fill="%s" fill-opacity=".16"/>
<path d="M350 253v234M163 270c62-9 115 2 154 31m-154 17c62-9 115 2 154 31m224-79c-62-9-115 2-154 31m154 17c-62-9-115 2-154 31"/>
</g>`, accent, secondary)
	case "house":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="5" stroke-linejoin="round" stroke-linecap="round" opacity=".84">
<path d="m133 332 217-174 217 174v190H133Z" fill="%s" fill-opacity=".13"/>
<path d="M215 522V365h105v157m67-128h93v82h-93zm-220-35h390M175 332h350"/>
<path d="M325 230v-62h70v118" opacity=".75"/>
</g>`, accent, secondary)
	case "city":
		return fmt.Sprintf(`<g stroke="%s" stroke-width="4" stroke-linejoin="round" opacity=".82">
<path d="M112 500V313h112v187m18 0V211h135v289m22 0V280h115v220m20 0V349h64v151Z" fill="%s" fill-opacity=".17"/>
<path d="M142 350h26m34 0h-26m-34 45h26m34 0h-26m95-140h26m42 0h-26m-42 48h26m42 0h-26m-42 48h26m42 0h-26m95 0h26m35 0h-26m-35 46h26m35 0h-26M108 500h490" fill="none" stroke-linecap="round"/>
</g>`, accent, secondary)
	case "forest":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".16" stroke="%s" stroke-width="4" stroke-linejoin="round" opacity=".84">
<path d="m147 444 71-155 71 155h-42l62 84H127l62-84Zm192-34 83-190 83 190h-49l72 98H316l72-98Zm150 45 62-136 62 136h-37l53 72H388l53-72Z"/>
<path d="M218 444v93m204-83v83m129-38v38" fill="none" stroke-linecap="round"/>
</g>`, secondary, accent)
	case "mountain":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".13" stroke="%s" stroke-width="5" stroke-linejoin="round" opacity=".84">
<path d="m82 500 177-253 105 141 94-191 160 303Z"/>
<path d="m213 313 46-66 44 59-30-9-14 23-17-19Zm205-26 40-90 42 79-30-13-17 23-13-18Z" fill="none"/>
<path d="M82 500h536" fill="none"/>
</g>`, secondary, accent)
	case "ship":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="5" stroke-linejoin="round" stroke-linecap="round" opacity=".84">
<path d="M145 433h410l-68 91H219Z" fill="%s" fill-opacity=".15"/>
<path d="M345 185v247m14-220 141 185H359Zm-19 50-129 135h129Zm-193 254c43-28 87-28 130 0s87 28 130 0 87-28 130 0 87 28 130 0"/>
</g>`, accent, secondary)
	case "flower":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".18" stroke="%s" stroke-width="4" stroke-linejoin="round" opacity=".82">
<path d="M350 338c-64-86-9-150 0-152 9 2 64 66 0 152Zm0 0c24-104 105-113 112-108 4 7-3 88-112 108Zm0 0c104-24 150 43 148 51-5 6-82 30-148-51Zm0 0c64 86 9 150 0 152-9-2-64-66 0-152Zm0 0c-24 104-105 113-112 108-4-7 3-88 112-108Zm0 0c-104 24-150-43-148-51 5-6 82-30 148 51Z"/>
<circle cx="350" cy="338" r="29"/><path d="M350 491v82m0-42-65-39m65 53 70-47" fill="none" stroke-linecap="round"/>
</g>`, secondary, accent)
	case "bird":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".15" stroke="%s" stroke-width="5" stroke-linecap="round" stroke-linejoin="round" opacity=".85">
<path d="M140 392c61-95 140-145 236-149-16 36-17 72 0 108 35-61 86-97 154-107-24 76-71 130-140 160-72 31-154 35-250 15Z"/>
<path d="M192 408c75-39 144-72 213-100m-122 97 24 80m64-101 62 58" fill="none"/>
</g>`, secondary, accent)
	case "horse":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".16" stroke="%s" stroke-width="5" stroke-linejoin="round" stroke-linecap="round" opacity=".84">
<path d="M214 508V348l57-116 108 35 62-57 90 50-34 76-8 172h-55l-14-126-82 30-21 96h-54l12-128-61 30v98Z"/>
<path d="m271 232-11-62 61 36m119 8 16 53m-111-10c20 12 41 12 61 0m-122 31 58-16m-83 237-40 30m168-30-6 30" fill="none"/>
<circle cx="387" cy="286" r="5" fill="%s" stroke="none"/>
</g>`, secondary, accent, accent)
	case "tree":
		return fmt.Sprintf(`<g stroke="%s" stroke-width="5" stroke-linejoin="round" opacity=".84">
<path d="M350 520V347m0 103-95-78m95 46 98-94m-98 44-12-106" fill="none" stroke-linecap="round"/>
<path d="M350 164c-52 0-85 38-80 82-48-4-76 31-64 69-43 17-51 68-21 96-13 49 24 84 71 74 23 40 77 39 94 6 32 36 89 29 101-15 53 1 77-43 57-83 32-37 15-84-23-96 4-48-34-76-76-66-5-39-31-67-59-67Z" fill="%s" fill-opacity=".2"/>
<path d="M350 520h0" fill="none"/>
</g>`, accent, secondary)
	case "lantern":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="5" stroke-linejoin="round" stroke-linecap="round" opacity=".84">
<path d="M310 188c0-54 80-54 80 0m-111 30h142l-22 42v199l-29 41h-40l-29-41V260Zm0 42h142m-126 180h110" fill="%s" fill-opacity=".14"/>
<path d="M350 282c-35 50-35 111 0 151 35-40 35-101 0-151Z" fill="%s" fill-opacity=".38"/>
</g>`, accent, secondary, secondary)
	case "moon":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="4" opacity=".82">
<path d="M394 174c-116 20-174 135-131 237 36 85 137 118 220 70-100 8-171-66-171-153 0-72 36-126 82-154Z" fill="%s" fill-opacity=".18"/>
<circle cx="493" cy="248" r="5" fill="%s"/><circle cx="550" cy="348" r="4" fill="%s"/><circle cx="434" cy="457" r="5" fill="%s"/>
<path d="m545 180 8 24 25 1-20 15 8 24-21-15-21 15 8-24-21-15 26-1z"/>
</g>`, accent, secondary, accent, accent, accent)
	case "key":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="12" stroke-linecap="round" stroke-linejoin="round" opacity=".84">
<circle cx="254" cy="325" r="87" fill="%s" fill-opacity=".12"/><circle cx="254" cy="325" r="39"/>
<path d="m319 390 222 222m-78-78 47-47m-91 3 47-47"/>
</g>`, accent, secondary)
	case "crown":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".19" stroke="%s" stroke-width="5" stroke-linejoin="round" opacity=".84">
<path d="m159 450 41-205 114 113 36-190 46 190 111-113 37 205-385 0Z"/>
<path d="M184 489h333v48H184zM200 277l52 122m211-122-50 122m-63-231v210" fill="none"/>
<circle cx="200" cy="245" r="12"/><circle cx="350" cy="168" r="12"/><circle cx="507" cy="245" r="12"/>
</g>`, secondary, accent)
	case "sword":
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="6" stroke-linejoin="round" stroke-linecap="round" opacity=".84">
<path d="m470 171-215 278 51 51 278-215-114-114Z" fill="%s" fill-opacity=".16"/>
<path d="m255 449-64 104m115-53-51 51m-13-115 99 99m-27-126 54 54"/>
</g>`, accent, secondary)
	case "mask":
		return fmt.Sprintf(`<g fill="%s" fill-opacity=".16" stroke="%s" stroke-width="5" stroke-linejoin="round" opacity=".84">
<path d="M140 236c65-39 146-39 211 0v139c-12 82-75 135-106 135s-94-53-105-135Z"/>
<path d="M350 236c65-39 146-39 211 0v139c-12 82-75 135-106 135s-94-53-105-135Z"/>
<path d="m188 319 55 8-47 28m195-28 55-8-47 36m-163 60c27 12 53 12 79 0m37 0c27-12 53-12 79 0" fill="none"/>
</g>`, secondary, accent)
	default:
		return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-width="4" opacity=".75">
<circle cx="350" cy="337" r="160"/><circle cx="350" cy="337" r="103" stroke="%s" stroke-dasharray="8 13"/>
<path d="M175 337h350M350 160v354m-124-302 248 250m0-250L226 462"/>
</g>`, accent, secondary)
	}
}

type personPosition struct {
	x, y  int
	scale float64
	hair  int
	class string
}

func peopleArt(accent, secondary string, people []personPosition) string {
	var art string
	for _, person := range people {
		art += personArt(accent, secondary, person)
	}
	return `<g opacity=".9">` + art + `</g>`
}

func personArt(accent, secondary string, person personPosition) string {
	hair := []string{
		`<path d="M-29 5c-9-42 13-64 38-59 25 4 32 28 23 62l-8 27-9-34c-12 5-24 6-38 3l-9 38Z" fill="%s"/>`,
		`<path d="M-30 4c-9-41 13-61 37-57 25 3 34 26 23 58-9-8-17-14-28-17-8 11-18 17-32 16Z" fill="%s"/>`,
		`<path d="M-28 5c-6-37 10-59 33-58 29 0 39 27 25 60l-8 26-9-32c-13 4-26 4-39-1l-5 34Z" fill="%s"/><circle cx="30" cy="-34" r="12" fill="%s"/>`,
		`<path d="M-31 6c-8-43 15-65 40-59 25 6 31 31 18 61-9-9-18-14-29-17-8 12-17 18-29 15Z" fill="%s"/><circle cx="-27" cy="-20" r="10" fill="%s"/><circle cx="26" cy="-24" r="10" fill="%s"/>`,
	}
	var hairstyle string
	switch person.hair % len(hair) {
	case 2:
		hairstyle = fmt.Sprintf(hair[2], accent, accent)
	case 3:
		hairstyle = fmt.Sprintf(hair[3], accent, accent, accent)
	default:
		hairstyle = fmt.Sprintf(hair[person.hair%len(hair)], accent)
	}
	return fmt.Sprintf(`<g class="%s" transform="translate(%d %d) scale(%.2f)">
<circle cx="0" cy="0" r="29" fill="%s" fill-opacity=".13" stroke="%s" stroke-width="3"/>
%s
<path d="M-14 27v22m28-22v22" fill="none" stroke="%s" stroke-width="4"/>
<path d="M-17 44c-31 8-43 28-49 74l-14 96c51 23 127 23 178 0l-14-96c-6-46-18-66-49-74l-17 19-18-19Z" fill="%s" fill-opacity=".2" stroke="%s" stroke-width="4" stroke-linejoin="round"/>
<path d="M0 62v139m-47-94h94" fill="none" stroke="%s" stroke-width="2.5" opacity=".75"/>
</g>`, person.class, person.x, person.y, person.scale, secondary, accent, hairstyle,
		accent, secondary, accent, accent)
}
