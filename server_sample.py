from flask import *

app = Flask(__name__)
@app.route("/", methods=["POST"])
def generate_text():
    input_text = request.form["message"]
    app.logger.info('*** INPUT TEXT BEGIN ***')
    app.logger.info(input_text)
    app.logger.info('*** INPUT TEXT END ***')

    response_head = input_text + " ですわ"

    app.logger.info('*** RESPONSE TEXT BEGIN ***')
    app.logger.info(response_head)
    app.logger.info('*** RESPONSE TEXT END ***')
    return response_head

if __name__ == "__main__":
    app.run(debug=True, host='0.0.0.0', port=3000, threaded=True)