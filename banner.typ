#let banner = json("data.json")

#set page(margin: 2cm)
#set text(size: 14pt)

#set align(center + horizon)

#text(size: 24pt, weight: "bold")[Print Job]
#v(1cm)

#for (key, value) in banner.data {
  if key.starts-with("img") {
    image(value, width: 50%)
    v(0.5cm)
  } else {
    text(weight: "bold")[#key: ]
    text[#value]
    linebreak()
  }
}
